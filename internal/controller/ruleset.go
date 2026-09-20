package controller

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/yuanying/cnidaria-cni/internal/netpol"
	"github.com/yuanying/cnidaria-cni/internal/nftables"
)

// Ruleset keeps this node's nftables table in step with what the API server says
// (ADR 0003, 0007). Every Pod, Namespace, NetworkPolicy and Node event maps to the
// same request, so the work queue collapses a burst of them into one recompute of the
// whole table, which is the debounce ADR 0003 asks for.
type Ruleset struct {
	// Reader is the manager's cache.
	client.Reader
	// NodeName is the node this daemon runs on: its pod CIDRs are the table's, and
	// only its own pods are enforced here (ADR 0003).
	NodeName string
	// SafePorts are the ports the rules a NodePolicy cannot remove keep open.
	SafePorts nftables.SafePorts
	// Applier replaces the table with the rendered text; nftables.Applier on a
	// node. It skips a text it has already applied, and Forget is how this
	// reconciler asks for one to go on again anyway.
	Applier interface {
		Apply(ctx context.Context, text string) (bool, error)
		Forget()
	}

	// lastApply is when nft last ran. The applier would otherwise skip a table it
	// has already applied for as long as nothing about it changed, and a table
	// somebody removed behind the daemon's back would stay gone.
	lastApply time.Time
}

// SetupWithManager registers the reconciler for everything the table is rendered from,
// all of it under one fixed key.
func (r *Ruleset) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "ruleset"}}}
		})
	return ctrl.NewControllerManagedBy(mgr).
		Named("ruleset").
		Watches(&corev1.Pod{}, one).
		Watches(&corev1.Namespace{}, one).
		Watches(&networkingv1.NetworkPolicy{}, one).
		Watches(&corev1.Node{}, one).
		Complete(r)
}

// Reconcile renders the whole table from the cache and applies it. A success asks to
// be run again after Resync, and that run puts the table on even though nothing about
// it changed, so a table removed behind the daemon's back comes back. Reconciles for
// one controller run one at a time, so lastApply needs no lock.
func (r *Ruleset) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	text, err := r.render(ctx, log)
	if err != nil {
		return ctrl.Result{}, err
	}
	if text == "" {
		return ctrl.Result{RequeueAfter: Resync}, nil
	}
	if time.Since(r.lastApply) >= Resync {
		r.Applier.Forget()
	}
	changed, err := r.Applier.Apply(ctx, text)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("apply the nftables table: %w", err)
	}
	if changed {
		r.lastApply = time.Now()
		log.Info("applied the nftables table")
	}
	return ctrl.Result{RequeueAfter: Resync}, nil
}

// render reads everything the table depends on and returns the text to apply, or ""
// when there is nothing to apply yet or nothing that can be applied.
//
// Rendering can fail on what the API server holds — a name nft cannot read, an
// ipBlock that is not a prefix. Retrying that would fail the same way, and applying
// what did render would be a table with a policy silently missing from it, so the
// failure is reported and the node keeps the table it is already running. The next
// change to the offending object brings another reconcile.
func (r *Ruleset) render(ctx context.Context, log logr.Logger) (string, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	var params nftables.Params
	params.SafePorts = r.SafePorts
	for i := range nodes.Items {
		cidrs := fromNode(&nodes.Items[i], log).PodCIDRs
		params.ClusterPodCIDRs = append(params.ClusterPodCIDRs, cidrs...)
		if nodes.Items[i].Name == r.NodeName {
			params.PodCIDRs = cidrs
		}
	}
	if len(params.PodCIDRs) == 0 {
		log.Info("this node has no pod CIDR yet; the table is not rendered", "node", r.NodeName)
		return "", nil
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces); err != nil {
		return "", fmt.Errorf("list namespaces: %w", err)
	}
	var policies networkingv1.NetworkPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return "", fmt.Errorf("list network policies: %w", err)
	}

	p := netpol.Params{NodeName: r.NodeName}
	for i := range pods.Items {
		p.Pods = append(p.Pods, fromPod(&pods.Items[i], log))
	}
	for i := range namespaces.Items {
		p.Namespaces = append(p.Namespaces, netpol.Namespace{
			Name: namespaces.Items[i].Name, Labels: namespaces.Items[i].Labels,
		})
	}
	for i := range policies.Items {
		policy, err := fromNetworkPolicy(&policies.Items[i])
		if err != nil {
			log.Error(err, "the node's table was not rendered; the table already on the node stays in place")
			return "", nil
		}
		p.Policies = append(p.Policies, policy)
	}

	ruleset, err := nftables.Render(params)
	if err == nil {
		err = netpol.Add(ruleset, p)
	}
	// The NodePolicy renderer adds to the same ruleset here, before it is applied.
	if err != nil {
		log.Error(err, "the node's table was not rendered; the table already on the node stays in place")
		return "", nil
	}
	return ruleset.String(), nil
}

// fromPod reads what the policy renderer needs off a Pod. A value that does not parse
// is logged and left out rather than failing the whole table: one odd pod should not
// take the policy of every other with it.
func fromPod(pod *corev1.Pod, log logr.Logger) netpol.Pod {
	out := netpol.Pod{
		Namespace:   pod.Namespace,
		Name:        pod.Name,
		NodeName:    pod.Spec.NodeName,
		Labels:      pod.Labels,
		HostNetwork: pod.Spec.HostNetwork,
		Terminated:  pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed,
	}
	for _, ip := range pod.Status.PodIPs {
		addr, err := netip.ParseAddr(ip.IP)
		if err != nil {
			log.Info("ignoring an unparsable pod IP", "pod", pod.Namespace+"/"+pod.Name, "ip", ip.IP)
			continue
		}
		out.IPs = append(out.IPs, addr)
	}
	for _, c := range containersThatServe(pod) {
		for _, port := range c.Ports {
			if port.Name == "" || port.ContainerPort < 1 || port.ContainerPort > 65535 {
				continue
			}
			out.Ports = append(out.Ports, netpol.NamedPort{
				Name:     port.Name,
				Port:     uint16(port.ContainerPort),
				Protocol: netpol.Protocol(port.Protocol),
			})
		}
	}
	return out
}

// containersThatServe are the containers whose named ports a policy can name: the
// ordinary ones and the init containers that keep running, which is what a sidecar is.
func containersThatServe(pod *corev1.Pod) []corev1.Container {
	out := pod.Spec.Containers
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			out = append(out, c)
		}
	}
	return out
}

// fromNetworkPolicy reads a NetworkPolicy into the value the renderer takes. Unlike a
// pod, a policy that cannot be read is not skipped: leaving it out would leave the
// pods it isolates open.
func fromNetworkPolicy(policy *networkingv1.NetworkPolicy) (netpol.NetworkPolicy, error) {
	out := netpol.NetworkPolicy{
		Namespace:   policy.Namespace,
		Name:        policy.Name,
		PodSelector: policy.Spec.PodSelector,
	}
	for _, t := range policy.Spec.PolicyTypes {
		out.PolicyTypes = append(out.PolicyTypes, netpol.PolicyType(t))
	}
	fail := func(err error) (netpol.NetworkPolicy, error) {
		return netpol.NetworkPolicy{}, fmt.Errorf("NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
	}
	for _, rule := range policy.Spec.Ingress {
		converted, err := fromRule(rule.From, rule.Ports)
		if err != nil {
			return fail(err)
		}
		out.Ingress = append(out.Ingress, converted)
	}
	for _, rule := range policy.Spec.Egress {
		converted, err := fromRule(rule.To, rule.Ports)
		if err != nil {
			return fail(err)
		}
		out.Egress = append(out.Egress, converted)
	}
	return out, nil
}

func fromRule(peers []networkingv1.NetworkPolicyPeer, ports []networkingv1.NetworkPolicyPort) (netpol.Rule, error) {
	var out netpol.Rule
	for _, peer := range peers {
		converted := netpol.Peer{PodSelector: peer.PodSelector, NamespaceSelector: peer.NamespaceSelector}
		if peer.IPBlock != nil {
			block, err := fromIPBlock(peer.IPBlock)
			if err != nil {
				return netpol.Rule{}, err
			}
			converted.IPBlock = block
		}
		out.Peers = append(out.Peers, converted)
	}
	for _, port := range ports {
		converted := netpol.Port{}
		if port.Protocol != nil {
			converted.Protocol = netpol.Protocol(*port.Protocol)
		}
		if port.Port != nil {
			if port.Port.Type == intstr.String {
				converted.Name = port.Port.StrVal
			} else if n := port.Port.IntValue(); n > 0 && n <= 65535 {
				converted.Number = uint16(n)
			} else {
				return netpol.Rule{}, fmt.Errorf("port %d is not a port number", n)
			}
		}
		if port.EndPort != nil {
			if *port.EndPort < 1 || *port.EndPort > 65535 {
				return netpol.Rule{}, fmt.Errorf("endPort %d is not a port number", *port.EndPort)
			}
			converted.EndPort = uint16(*port.EndPort)
		}
		out.Ports = append(out.Ports, converted)
	}
	return out, nil
}

func fromIPBlock(block *networkingv1.IPBlock) (*netpol.IPBlock, error) {
	cidr, err := netip.ParsePrefix(block.CIDR)
	if err != nil {
		return nil, fmt.Errorf("ipBlock cidr %q: %w", block.CIDR, err)
	}
	out := &netpol.IPBlock{CIDR: cidr.Masked()}
	for _, e := range block.Except {
		except, err := netip.ParsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("ipBlock except %q: %w", e, err)
		}
		out.Except = append(out.Except, except.Masked())
	}
	return out, nil
}

// StripPod is the cache transform for Pods. Every node's daemon holds every pod in
// the cluster, because a policy peer may name a pod on any node (ADR 0007), so what
// is kept of each is only what the renderer reads: the identity the cache indexes by,
// the labels a selector matches, the node, the phase, the addresses, and the named
// ports. On a small control-plane node the annotations and the container specs of a
// few hundred pods are the difference that matters.
func StripPod(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// A cache hands the transform tombstones and other things too.
		return obj, nil
	}
	stripped := &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       pod.Namespace,
			Name:            pod.Name,
			UID:             pod.UID,
			ResourceVersion: pod.ResourceVersion,
			Labels:          pod.Labels,
		},
		Spec: corev1.PodSpec{
			NodeName:    pod.Spec.NodeName,
			HostNetwork: pod.Spec.HostNetwork,
		},
		Status: corev1.PodStatus{Phase: pod.Status.Phase, PodIPs: pod.Status.PodIPs},
	}
	for _, c := range pod.Spec.Containers {
		stripped.Spec.Containers = append(stripped.Spec.Containers, keepPorts(c))
	}
	for _, c := range pod.Spec.InitContainers {
		stripped.Spec.InitContainers = append(stripped.Spec.InitContainers, keepPorts(c))
	}
	return stripped, nil
}

func keepPorts(c corev1.Container) corev1.Container {
	return corev1.Container{Name: c.Name, RestartPolicy: c.RestartPolicy, Ports: c.Ports}
}
