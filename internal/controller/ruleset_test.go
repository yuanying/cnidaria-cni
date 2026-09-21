package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yuanying/cnidaria-cni/internal/apis/v1alpha1"
	"github.com/yuanying/cnidaria-cni/internal/nftables"
)

// applied keeps the text the reconciler handed to nft, skipping a repeat of the last
// one as nftables.Applier does.
type applied struct {
	texts    []string
	previous string
	fail     error
}

func (a *applied) Apply(_ context.Context, text string) (bool, error) {
	if a.fail != nil {
		return false, a.fail
	}
	if text == a.previous {
		return false, nil
	}
	a.previous = text
	a.texts = append(a.texts, text)
	return true, nil
}

func (a *applied) Forget() { a.previous = "" }

func (a *applied) last(t *testing.T) string {
	t.Helper()
	if len(a.texts) == 0 {
		t.Fatal("nothing was applied")
	}
	return a.texts[len(a.texts)-1]
}

// testLogger sends what the reconciler logs to the test's own output.
func testLogger(t *testing.T) logr.Logger {
	t.Helper()
	return funcr.New(func(_, args string) { t.Log(args) }, funcr.Options{})
}

func ptr[T any](v T) *T { return &v }

func testPod(namespace, name, node string, labels map[string]string, ips ...string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
	}
	for _, ip := range ips {
		p.Status.PodIPs = append(p.Status.PodIPs, corev1.PodIP{IP: ip})
	}
	return p
}

func testNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func newRulesetReconciler(t *testing.T, a *applied, objs ...client.Object) *Ruleset {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{scheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.NodeNetworkPolicy{}).
		Build()
	return &Ruleset{Client: c, NodeName: "node-a", SafePorts: nftables.DefaultSafePorts, Applier: a}
}

// cluster is the objects every case below starts from.
func cluster() []client.Object {
	return []client.Object{
		node("node-a", []string{"192.0.2.0/24", "2001:db8:1::/64"}, "203.0.113.1", "2001:db8::1"),
		node("node-b", []string{"198.51.100.0/24", "2001:db8:2::/64"}, "203.0.113.2", "2001:db8::2"),
		testNamespace("default", map[string]string{"kubernetes.io/metadata.name": "default"}),
		testNamespace("other", map[string]string{"tier": "trusted"}),
		testPod("default", "web", "node-a", map[string]string{"app": "web"}, "192.0.2.10", "2001:db8:1::10"),
		testPod("default", "client", "node-a", map[string]string{"app": "client"}, "192.0.2.11", "2001:db8:1::11"),
		testPod("other", "probe", "node-b", map[string]string{"app": "probe"}, "198.51.100.11", "2001:db8:2::11"),
	}
}

func mustReconcile(t *testing.T, r *Ruleset) {
	t.Helper()
	if _, err := r.Reconcile(ctrl.LoggerInto(t.Context(), testLogger(t)), ctrl.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// One reconcile reads every object from the cache, renders this node's table from the
// node CIDRs and the policies, and applies it (ADR 0003, 0007).
func TestRulesetReconcileRendersAndApplies(t *testing.T) {
	a := &applied{}
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(80)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-in"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "client"}}},
					{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "trusted"}}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24", Except: []string{"203.0.113.128/25"}}},
				},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
	r := newRulesetReconciler(t, a, append(cluster(), policy)...)
	mustReconcile(t, r)

	got := a.last(t)
	for _, want := range []string{
		"elements = { 192.0.2.0/24, 198.51.100.0/24 }",
		`ip daddr @pods_default/web-in/v4 counter jump ingress_default/web-in comment "default/web-in"`,
		`ip saddr @peer_default/web-in/in0.0/v4 tcp dport 80 counter accept comment "default/web-in"`,
		`ip saddr @peer_default/web-in/in0.1/v4 tcp dport 80 counter accept comment "default/web-in"`,
		`ip saddr 203.0.113.0/24 ip saddr != { 203.0.113.128/25 } tcp dport 80 counter accept comment "default/web-in"`,
		"set isolated_ingress_v4 {\n\t\ttype ipv4_addr\n\t\telements = { 192.0.2.10 }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the applied table lacks %q:\n%s", want, got)
		}
	}
}

// A named port is read off the pod's container ports, sidecars included.
func TestRulesetResolvesNamedPorts(t *testing.T) {
	a := &applied{}
	always := corev1.ContainerRestartPolicyAlways
	web := testPod("default", "web", "node-a", map[string]string{"app": "web"}, "192.0.2.10")
	web.Spec.Containers = []corev1.Container{{
		Name:  "app",
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}}
	web.Spec.InitContainers = []corev1.Container{
		{Name: "setup", Ports: []corev1.ContainerPort{{Name: "never", ContainerPort: 1}}},
		{
			Name: "proxy", RestartPolicy: &always,
			Ports: []corev1.ContainerPort{{Name: "admin", ContainerPort: 9901, Protocol: corev1.ProtocolTCP}},
		},
	}
	named := intstr.FromString("http")
	admin := intstr.FromString("admin")
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-in"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{Ports: []networkingv1.NetworkPolicyPort{{Port: &named}}},
				{Ports: []networkingv1.NetworkPolicyPort{{Port: &admin}}},
			},
		},
	}
	objs := append(cluster(), policy)
	objs[4] = web // the web pod, with its ports
	r := newRulesetReconciler(t, a, objs...)
	mustReconcile(t, r)

	got := a.last(t)
	for _, want := range []string{
		`ip daddr @port_default/web-in/in0.0.0/8080/v4 tcp dport 8080 counter accept`,
		`ip daddr @port_default/web-in/in1.0.0/9901/v4 tcp dport 9901 counter accept`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the applied table lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "dport 1 ") {
		t.Error("a port of an init container that does not keep running was opened")
	}
}

// A policy the renderer cannot turn into a table is a fault in what the API server
// holds, not in the node. The reconcile reports it and leaves the table the node is
// already running, rather than applying a table with the policy missing from it.
func TestRulesetKeepsTheTableWhenRenderingFails(t *testing.T) {
	cases := map[string]*networkingv1.NetworkPolicy{
		"an endPort below the port it starts from": {
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bad-range"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(200)), EndPort: ptr(int32(100))}},
				}},
			},
		},
		"an ipBlock that is not a prefix": {
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bad-block"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "not a cidr"}}},
				}},
			},
		},
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			a := &applied{}
			r := newRulesetReconciler(t, a, append(cluster(), policy)...)
			mustReconcile(t, r)
			if len(a.texts) != 0 {
				t.Errorf("a table was applied although rendering failed:\n%s", a.texts[0])
			}
		})
	}
}

// Until the controller manager has given this node a pod CIDR there is no table to
// render. Waiting is not a failure.
func TestRulesetWaitsForThisNodesPodCIDR(t *testing.T) {
	a := &applied{}
	r := newRulesetReconciler(t, a, node("node-a", nil, "203.0.113.1"))
	mustReconcile(t, r)
	if len(a.texts) != 0 {
		t.Errorf("a table was applied for a node with no pod CIDR:\n%s", a.texts[0])
	}
}

// A failing nft is the node's problem and has to be retried, so it comes back as an
// error and the queue backs off.
func TestRulesetReturnsWhatNFTRefused(t *testing.T) {
	a := &applied{fail: errors.New("nft said no")}
	r := newRulesetReconciler(t, a, cluster()...)
	if _, err := r.Reconcile(ctrl.LoggerInto(t.Context(), testLogger(t)), ctrl.Request{}); err == nil {
		t.Error("Reconcile hid a failed apply")
	}
}

// Every node in the cluster holds every pod in the cluster in its cache, because a
// policy peer may name a pod on any node (ADR 0007). What is kept of each is only what
// the renderer reads.
func TestStripPodKeepsOnlyWhatTheRendererReads(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := testPod("default", "web", "node-a", map[string]string{"app": "web"}, "192.0.2.10")
	pod.UID = "0f1b"
	pod.ResourceVersion = "42"
	pod.Status.Phase = corev1.PodRunning
	pod.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": strings.Repeat("x", 4096)}
	pod.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubelet"}}
	pod.Spec.HostNetwork = true
	pod.Spec.Volumes = []corev1.Volume{{Name: "data"}}
	pod.Spec.Containers = []corev1.Container{{
		Name:  "app",
		Image: "example/app:1",
		Env:   []corev1.EnvVar{{Name: "SECRET", Value: strings.Repeat("y", 4096)}},
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
	}}
	pod.Spec.InitContainers = []corev1.Container{{Name: "proxy", RestartPolicy: &always, Image: "example/proxy:1"}}

	out, err := StripPod(pod.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	stripped, ok := out.(*corev1.Pod)
	if !ok {
		t.Fatalf("StripPod returned %T", out)
	}
	if stripped.Name != "web" || stripped.Namespace != "default" || stripped.UID != "0f1b" || stripped.ResourceVersion != "42" {
		t.Errorf("the cache's own fields did not survive: %+v", stripped.ObjectMeta)
	}
	for what, kept := range map[string]int{
		"annotations":                    len(stripped.Annotations),
		"managedFields":                  len(stripped.ManagedFields),
		"volumes":                        len(stripped.Spec.Volumes),
		"the image of a container":       len(stripped.Spec.Containers[0].Image),
		"the environment of a container": len(stripped.Spec.Containers[0].Env),
	} {
		if kept != 0 {
			t.Errorf("bulk the renderer never reads survived: %s", what)
		}
	}
	// What the renderer reads has to come out the same on either side of the transform.
	log := testLogger(t)
	if got, want := fromPod(stripped, log), fromPod(pod, log); !reflect.DeepEqual(got, want) {
		t.Errorf("the stripped pod renders differently:\ngot  %+v\nwant %+v", got, want)
	}
	// Anything that is not a pod passes through: a cache hands the transform
	// tombstones too.
	if out, err := StripPod("not a pod"); err != nil || out != "not a pod" {
		t.Errorf("StripPod(%q) = %v, %v", "not a pod", out, err)
	}
}

// Between resyncs a reconcile that renders the same table again does not reach nft.
// On the resync it does, so a table someone flushed behind the daemon's back is put
// back, which is what the routes reconciler does for routes (ADR 0006).
func TestRulesetPutsAnUnchangedTableBackOnTheResync(t *testing.T) {
	a := &applied{}
	r := newRulesetReconciler(t, a, cluster()...)
	mustReconcile(t, r)
	mustReconcile(t, r)
	if len(a.texts) != 1 {
		t.Fatalf("nft was run %d times for an unchanged table, want 1", len(a.texts))
	}
	r.lastApply = time.Now().Add(-Resync - time.Second)
	mustReconcile(t, r)
	if len(a.texts) != 2 {
		t.Errorf("the resync did not put the table back: nft was run %d times, want 2", len(a.texts))
	}
}

// A pod that has run to its end keeps its addresses until something deletes it, and
// by then one of them may belong to a pod that is running. It is left out of the
// table rather than granting the new pod what the dead one's labels asked for.
func TestRulesetIgnoresPodsThatHaveFinished(t *testing.T) {
	for phase, want := range map[corev1.PodPhase]bool{
		corev1.PodRunning:   true,
		corev1.PodPending:   true,
		corev1.PodSucceeded: false,
		corev1.PodFailed:    false,
	} {
		t.Run(string(phase), func(t *testing.T) {
			a := &applied{}
			objs := cluster()
			web := testPod("default", "web", "node-a", map[string]string{"app": "web"}, "192.0.2.10")
			web.Status.Phase = phase
			objs[4] = web
			policy := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-in"},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				},
			}
			r := newRulesetReconciler(t, a, append(objs, policy)...)
			mustReconcile(t, r)

			got := strings.Contains(a.last(t), "elements = { 192.0.2.10 }")
			if got != want {
				t.Errorf("a %s pod is in the table: %v, want %v", phase, got, want)
			}
		})
	}
}

// withLabels gives the named node its labels, which is what a NodeNetworkPolicy selects on.
func withLabels(objs []client.Object, name string, labels map[string]string) []client.Object {
	for _, o := range objs {
		if n, ok := o.(*corev1.Node); ok && n.Name == name {
			n.Labels = labels
		}
	}
	return objs
}

// workers selects the node the reconciler runs on in the cluster below.
var workers = metav1.LabelSelector{MatchLabels: map[string]string{"role": "worker"}}

func nodeNetworkPolicyCluster() []client.Object {
	return withLabels(cluster(), "node-a", map[string]string{"role": "worker"})
}

func metricsPolicy(mode v1alpha1.Mode) *v1alpha1.NodeNetworkPolicy {
	return &v1alpha1.NodeNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "metrics", Generation: 3},
		Spec: v1alpha1.NodeNetworkPolicySpec{
			Mode:         mode,
			NodeSelector: workers,
			Ingress: []v1alpha1.NodeNetworkPolicyIngressRule{{
				From:  []v1alpha1.NodeNetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24"}}},
				Ports: []v1alpha1.NodeNetworkPolicyPort{{Protocol: corev1.ProtocolTCP, Port: ptr(int32(9100))}},
			}},
		},
	}
}

func policyOf(t *testing.T, r *Ruleset, name string) v1alpha1.NodeNetworkPolicy {
	t.Helper()
	var policy v1alpha1.NodeNetworkPolicy
	if err := r.Get(t.Context(), client.ObjectKey{Name: name}, &policy); err != nil {
		t.Fatalf("reading back NodeNetworkPolicy %s: %v", name, err)
	}
	return policy
}

func statusOf(t *testing.T, r *Ruleset, name string) []v1alpha1.NodeNetworkPolicyNodeStatus {
	t.Helper()
	return policyOf(t, r, name).Status.Nodes
}

// A NodeNetworkPolicy goes into the same table as everything else, behind the safe rules it
// cannot remove, and what it did on this node goes into its status (ADR 0004).
func TestRulesetRendersNodeNetworkPolicyAndReportsWhatItDid(t *testing.T) {
	a := &applied{}
	r := newRulesetReconciler(t, a, append(nodeNetworkPolicyCluster(), metricsPolicy(v1alpha1.ModeEnforce))...)
	mustReconcile(t, r)

	got := a.last(t)
	for _, want := range []string{
		`ip saddr 203.0.113.0/24 tcp dport 9100 counter accept comment "nodenetworkpolicy metrics"`,
		`counter jump nodepol_in_metrics comment "nodenetworkpolicy metrics"`,
		`counter jump input_dispatch comment "nodenetworkpolicy: everything the safe rules did not accept"`,
		`counter drop comment "nodenetworkpolicy: no rule accepted"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the applied table lacks %q:\n%s", want, got)
		}
	}
	// The jump into the policy comes after the safe rules, never before them.
	if strings.Index(got, `comment "safe: kubelet"`) > strings.Index(got, "jump input_dispatch") {
		t.Errorf("the NodeNetworkPolicy jump sits ahead of the safe rules:\n%s", got)
	}

	want := []v1alpha1.NodeNetworkPolicyNodeStatus{
		{Name: "node-a", ObservedGeneration: 3, Mode: v1alpha1.ModeEnforce},
	}
	if diff := statusOf(t, r, "metrics"); !reflect.DeepEqual(diff, want) {
		t.Errorf("status.nodes = %+v, want %+v", diff, want)
	}
}

// Every selected node writes to the same object, so a node writes its own entry and
// leaves the others exactly as they are. A node no longer selected takes its own
// entry out rather than leaving a mode behind that is not in force.
func TestRulesetWritesOnlyItsOwnEntryInTheStatus(t *testing.T) {
	a := &applied{}
	elsewhere := v1alpha1.NodeNetworkPolicyNodeStatus{Name: "node-b", ObservedGeneration: 1, Mode: v1alpha1.ModeEnforce}
	policy := metricsPolicy(v1alpha1.ModeEnforce)
	policy.Spec.NodeSelector = metav1.LabelSelector{MatchLabels: map[string]string{"role": "storage"}}
	policy.Status.Nodes = []v1alpha1.NodeNetworkPolicyNodeStatus{
		{Name: "node-a", ObservedGeneration: 1, Mode: v1alpha1.ModeEnforce},
		elsewhere,
	}
	r := newRulesetReconciler(t, a, append(nodeNetworkPolicyCluster(), policy)...)
	mustReconcile(t, r)

	want := []v1alpha1.NodeNetworkPolicyNodeStatus{elsewhere}
	if got := statusOf(t, r, "metrics"); !reflect.DeepEqual(got, want) {
		t.Errorf("status.nodes = %+v, want %+v", got, want)
	}
	if got := a.last(t); strings.Contains(got, "input_dispatch") {
		t.Errorf("a policy that does not select this node closed a direction on it:\n%s", got)
	}
}

// A policy that cannot be rendered is reported in that policy's own status, and the
// node keeps the table it is already running rather than one with the policy missing.
func TestRulesetReportsANodeNetworkPolicyItCannotRender(t *testing.T) {
	a := &applied{}
	policy := metricsPolicy(v1alpha1.ModeEnforce)
	policy.Spec.Ingress[0].From[0].IPBlock.CIDR = "203.0.113.1/24" // bits below the prefix
	r := newRulesetReconciler(t, a, append(nodeNetworkPolicyCluster(), policy)...)
	mustReconcile(t, r)

	if len(a.texts) != 0 {
		t.Errorf("a table was applied although a NodeNetworkPolicy could not be rendered:\n%s", a.texts[0])
	}
	got := statusOf(t, r, "metrics")
	if len(got) != 1 || got[0].Name != "node-a" || got[0].Message == "" {
		t.Fatalf("status.nodes = %+v, want one entry for node-a carrying a message", got)
	}
	if got[0].Mode != "" {
		t.Errorf("status says the policy took effect in %q although it was not applied", got[0].Mode)
	}
	if got[0].ObservedGeneration != 3 {
		t.Errorf("observedGeneration = %d, want the generation that was refused, 3", got[0].ObservedGeneration)
	}
	if !strings.Contains(got[0].Message, "203.0.113.1/24") {
		t.Errorf("the message does not say what is wrong: %q", got[0].Message)
	}
	// The message sits in the status of the policy it is about, so naming that
	// policy again in it says nothing.
	if strings.Contains(got[0].Message, "metrics") {
		t.Errorf("the message repeats the name of the policy it is attached to: %q", got[0].Message)
	}
}

// A policy that does not select this node and holds no entry from it is not written
// to at all. Every Pod event reaches every node's reconciler, so a write here would
// be a PUT from every node that a policy does not select, on every event.
func TestRulesetDoesNotWriteAPolicyItHasNothingToSayAbout(t *testing.T) {
	a := &applied{}
	policy := metricsPolicy(v1alpha1.ModeEnforce)
	policy.Spec.NodeSelector = metav1.LabelSelector{MatchLabels: map[string]string{"role": "storage"}}
	policy.Status.Nodes = []v1alpha1.NodeNetworkPolicyNodeStatus{
		{Name: "node-b", ObservedGeneration: 3, Mode: v1alpha1.ModeEnforce},
	}
	r := newRulesetReconciler(t, a, append(nodeNetworkPolicyCluster(), policy)...)

	before := policyOf(t, r, "metrics").ResourceVersion
	mustReconcile(t, r)
	if after := policyOf(t, r, "metrics").ResourceVersion; after != before {
		t.Errorf("the status of a policy this node has nothing to say about was written: %s then %s",
			before, after)
	}
}

// The mode in the status is the mode the node is running (ADR 0004), so it is written
// after the table is on the node and not at all when nft refused it.
func TestRulesetWritesNoStatusWhenTheTableWasNotApplied(t *testing.T) {
	a := &applied{fail: errors.New("nft said no")}
	r := newRulesetReconciler(t, a, append(nodeNetworkPolicyCluster(), metricsPolicy(v1alpha1.ModeEnforce))...)
	if _, err := r.Reconcile(ctrl.LoggerInto(t.Context(), testLogger(t)), ctrl.Request{}); err == nil {
		t.Fatal("Reconcile: nft refused the table and no error came back")
	}
	if got := statusOf(t, r, "metrics"); len(got) != 0 {
		t.Errorf("status.nodes = %+v, want nothing: the table never went on", got)
	}
}

// Every node writes to the same object, so a write that changes nothing is a resource
// version bump every other node has to take.
func TestRulesetDoesNotRewriteAStatusItDidNotChange(t *testing.T) {
	a := &applied{}
	r := newRulesetReconciler(t, a, append(nodeNetworkPolicyCluster(), metricsPolicy(v1alpha1.ModePermissive))...)
	mustReconcile(t, r)

	var first v1alpha1.NodeNetworkPolicy
	if err := r.Get(t.Context(), client.ObjectKey{Name: "metrics"}, &first); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r)
	var second v1alpha1.NodeNetworkPolicy
	if err := r.Get(t.Context(), client.ObjectKey{Name: "metrics"}, &second); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("the status was written again although nothing about it changed: %s then %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}
