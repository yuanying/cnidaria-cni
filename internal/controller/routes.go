// Package controller holds the controller-runtime reconcilers (ADR 0007): the route
// reconciler driven by Node events, which also keeps the iptables forward chain and
// writes this node's conflist, and the ruleset reconciler that every Pod, Namespace,
// NetworkPolicy and NodeNetworkPolicy event funnels into one recompute of the node's
// nftables table.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/yuanying/cnidaria-cni/internal/conflist"
	"github.com/yuanying/cnidaria-cni/internal/routes"
)

// Resync is how often the route set is reapplied when no Node changes. A route
// deleted behind the daemon's back is put back by the next reconcile, and this is
// what bounds the wait for one (ADR 0006).
const Resync = 5 * time.Minute

// Routes keeps the host-gw routes (ADR 0006) and this node's conflist (ADR 0001) in
// step with the cluster's Node objects. Every Node event maps to the same request,
// so a burst of events becomes one recompute of the whole set.
type Routes struct {
	// Reader is the manager's cache.
	client.Reader
	// NodeName is the node this daemon runs on: its own object is what the conflist
	// is rendered from, and its own CIDRs get no route.
	NodeName string
	// Kernel applies the computed route set; routes.Kernel on a node.
	Kernel interface {
		Apply([]routes.Route) error
	}
	// Forward keeps the iptables chain that accepts traffic from and to every
	// node's pod CIDRs ahead of a FORWARD policy of DROP; iptables.Forward on a
	// node (ADR 0003).
	Forward interface {
		Apply(ctx context.Context, cidrs []netip.Prefix) error
	}
	Conflist Conflist
}

// Conflist is what the rendered list depends on besides the node's CIDRs.
type Conflist struct {
	// Path is where the list is written, in the runtime's CNI configuration directory.
	Path string
	// Name is the network name, which also names host-local's lease directory
	// (ADR 0009); Bridge the Linux bridge the pods attach to.
	Name, Bridge string
	// MTU is set on the bridge and every veth. Zero means read it from the interface
	// that holds one of this node's InternalIPs, which is the uplink pod traffic
	// leaves through (ADR 0001); an explicit value is for a node whose uplink is
	// not the interface the InternalIP sits on.
	MTU int
}

// SetupWithManager registers the reconciler for every Node event under one fixed key.
func (r *Routes) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("routes").
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(
			func(context.Context, client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "routes"}}}
			})).
		Complete(r)
}

// Reconcile recomputes the full route set from every Node in the cache and applies
// it, hands every node's pod CIDRs to the iptables forward chain, then writes the
// conflist from this node's own object. Each step runs even if another fails, and
// every failure is returned so that the queue retries with backoff. A success asks to
// be run again after Resync.
func (r *Routes) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var list corev1.NodeList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodes: %w", err)
	}
	nodes := make([]routes.Node, 0, len(list.Items))
	var clusterPodCIDRs []netip.Prefix
	for i := range list.Items {
		n := fromNode(&list.Items[i], log)
		nodes = append(nodes, n)
		clusterPodCIDRs = append(clusterPodCIDRs, n.PodCIDRs...)
	}

	set, missing := routes.Compute(r.NodeName, nodes)
	for _, m := range missing {
		log.Info("no route for a family: "+m.String(), "node", m.Node, "family", m.Family)
	}
	applyErr := r.Kernel.Apply(set)
	forwardErr := r.Forward.Apply(ctx, clusterPodCIDRs)

	var writeErr error
	if i := slices.IndexFunc(nodes, func(n routes.Node) bool { return n.Name == r.NodeName }); i >= 0 {
		writeErr = r.writeConflist(&nodes[i], log)
	}
	if err := errors.Join(applyErr, forwardErr, writeErr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: Resync}, nil
}

func (r *Routes) writeConflist(self *routes.Node, log logr.Logger) error {
	if len(self.PodCIDRs) == 0 {
		log.Info("this node has no pod CIDR yet; conflist not written", "node", self.Name)
		return nil
	}
	mtu := r.Conflist.MTU
	if mtu == 0 {
		var err error
		if mtu, err = uplinkMTU(self.InternalIPs); err != nil {
			return fmt.Errorf("conflist: %w", err)
		}
	}
	data, err := conflist.Render(conflist.Params{
		Name:     r.Conflist.Name,
		Bridge:   r.Conflist.Bridge,
		MTU:      mtu,
		PodCIDRs: self.PodCIDRs,
	})
	if err != nil {
		return err
	}
	changed, err := conflist.WriteFile(r.Conflist.Path, data)
	if err != nil {
		return err
	}
	if changed {
		log.Info("wrote conflist", "path", r.Conflist.Path, "podCIDRs", self.PodCIDRs, "mtu", mtu)
	}
	return nil
}

// fromNode reads the two things the route computation needs off a Node object. A
// value that does not parse is logged and skipped rather than failing the whole
// reconcile, since one odd node should not take the routes to every other down.
func fromNode(n *corev1.Node, log logr.Logger) routes.Node {
	out := routes.Node{Name: n.Name}
	for _, s := range n.Spec.PodCIDRs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			log.Info("ignoring an unparsable pod CIDR", "node", n.Name, "podCIDR", s, "error", err.Error())
			continue
		}
		out.PodCIDRs = append(out.PodCIDRs, p)
	}
	for _, a := range n.Status.Addresses {
		if a.Type != corev1.NodeInternalIP {
			continue
		}
		ip, err := netip.ParseAddr(a.Address)
		if err != nil {
			log.Info("ignoring an unparsable InternalIP", "node", n.Name, "address", a.Address, "error", err.Error())
			continue
		}
		out.InternalIPs = append(out.InternalIPs, ip)
	}
	return out
}

// uplinkMTU returns the MTU of the first interface that holds one of the addresses.
func uplinkMTU(addrs []netip.Addr) (int, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, err
	}
	for _, iface := range ifaces {
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, ia := range ifaceAddrs {
			ipnet, ok := ia.(*net.IPNet)
			if !ok {
				continue
			}
			held, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			for _, a := range addrs {
				if held.Unmap() == a.Unmap() {
					return iface.MTU, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("no interface holds any of this node's InternalIPs %v; set --mtu explicitly", addrs)
}
