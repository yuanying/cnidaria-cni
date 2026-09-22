package controller

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/yuanying/cnidaria-cni/internal/conflist"
	"github.com/yuanying/cnidaria-cni/internal/routes"
)

// recorder stands in for the netlink applier and keeps what it was given.
type recorder struct {
	got  []routes.Route
	fail error
}

func (r *recorder) Apply(set []routes.Route) error {
	r.got = set
	return r.fail
}

// forwardRecorder stands in for the iptables forward chain and keeps what it was given.
type forwardRecorder struct {
	got  []netip.Prefix
	fail error
}

func (f *forwardRecorder) Apply(_ context.Context, cidrs []netip.Prefix) error {
	f.got = cidrs
	return f.fail
}

func node(name string, podCIDRs []string, internalIPs ...string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{PodCIDRs: podCIDRs},
	}
	// A Hostname entry sits first, as it does on a real node, so that the test
	// shows only InternalIP entries are read.
	n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeHostName, Address: name})
	for _, ip := range internalIPs {
		n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
	}
	return n
}

func newReconciler(t *testing.T, kernel *recorder, objs ...client.Object) (*Routes, string) {
	t.Helper()
	return newReconcilerWithForward(t, kernel, &forwardRecorder{}, objs...)
}

func newReconcilerWithForward(t *testing.T, kernel *recorder, forward *forwardRecorder, objs ...client.Object) (*Routes, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "10-cnidaria.conflist")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	return &Routes{
		Reader:   c,
		NodeName: "node-a",
		Kernel:   kernel,
		Forward:  forward,
		Conflist: Conflist{Path: path, Name: "cnidaria", Bridge: "cni0", MTU: 1500},
	}, path
}

// One reconcile reads every Node, hands the kernel the full route set (ADR 0006) and
// writes this node's conflist from its own CIDRs (ADR 0001).
func TestReconcileAppliesRoutesAndWritesTheConflist(t *testing.T) {
	kernel := &recorder{}
	r, path := newReconciler(t, kernel,
		node("node-a", []string{"192.0.2.0/24", "2001:db8:a::/64"}, "203.0.113.1", "2001:db8::1"),
		node("node-b", []string{"198.51.100.0/24", "2001:db8:b::/64"}, "203.0.113.2", "2001:db8::2"),
		node("node-c", []string{"203.0.113.128/25"}, "203.0.113.3"),
	)
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	want := []routes.Route{
		{Node: "node-b", Dst: netip.MustParsePrefix("198.51.100.0/24"), Via: netip.MustParseAddr("203.0.113.2")},
		{Node: "node-b", Dst: netip.MustParsePrefix("2001:db8:b::/64"), Via: netip.MustParseAddr("2001:db8::2")},
		{Node: "node-c", Dst: netip.MustParsePrefix("203.0.113.128/25"), Via: netip.MustParseAddr("203.0.113.3")},
	}
	if diff := cmp.Diff(want, kernel.got, cmp.Comparer(func(a, b netip.Prefix) bool { return a == b }),
		cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Errorf("routes handed to the kernel differ (-want +got):\n%s", diff)
	}

	wantList, err := conflist.Render(conflist.Params{
		Name: "cnidaria", Bridge: "cni0", MTU: 1500,
		PodCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:a::/64")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("conflist was not written: %v", err)
	}
	if string(got) != string(wantList) {
		t.Errorf("conflist differs\n--- got ---\n%s\n--- want ---\n%s", got, wantList)
	}
}

// The ipam store name reaches the conflist, so that a node migrating from another
// CNI shares that CNI's leases (ADR 0009).
func TestReconcileWritesTheIPAMNameIntoTheConflist(t *testing.T) {
	r, path := newReconciler(t, &recorder{}, node("node-a", []string{"192.0.2.0/24"}, "203.0.113.1"))
	r.Conflist.IPAMName = "cbr0"
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"name": "cbr0"`) {
		t.Errorf("conflist does not carry the ipam name:\n%s", got)
	}
}

// Before this node's own object is in the cache there is nothing to render. Routes
// to the peers that are there are still installed.
func TestReconcileWithoutOwnNodeInstallsRoutesButNoConflist(t *testing.T) {
	kernel := &recorder{}
	r, path := newReconciler(t, kernel, node("node-b", []string{"198.51.100.0/24"}, "203.0.113.2"))
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(kernel.got) != 1 {
		t.Errorf("kernel got %v, want the one route to node-b", kernel.got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("conflist exists (stat: %v), want none without this node's CIDRs", err)
	}
}

// A route that could not be installed comes back as an error so that the queue
// retries with backoff, and the conflist is still written in the same pass.
func TestReconcileReturnsKernelFailures(t *testing.T) {
	kernel := &recorder{fail: errors.New("install 198.51.100.0/24 via 203.0.113.2 (node node-b): network is unreachable")}
	r, path := newReconciler(t, kernel,
		node("node-a", []string{"192.0.2.0/24"}, "203.0.113.1"),
		node("node-b", []string{"198.51.100.0/24"}, "203.0.113.2"),
	)
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	if err == nil || !strings.Contains(err.Error(), "node-b") {
		t.Errorf("Reconcile returned %v, want the kernel's error naming node-b", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("conflist was not written when a route failed: %v", statErr)
	}
}

// When both the kernel and the conflist fail, neither failure hides the other.
func TestReconcileReturnsBothKernelAndConflistFailures(t *testing.T) {
	kernel := &recorder{fail: errors.New("install 198.51.100.0/24 via 203.0.113.2 (node node-b): network is unreachable")}
	r, _ := newReconciler(t, kernel,
		node("node-a", []string{"192.0.2.0/24"}, "203.0.113.1"),
		node("node-b", []string{"198.51.100.0/24"}, "203.0.113.2"),
	)
	r.Conflist.Path = filepath.Join(t.TempDir(), "missing", "10-cnidaria.conflist")
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	if err == nil {
		t.Fatal("Reconcile returned nil with both the kernel and the conflist failing")
	}
	for _, want := range []string{"node-b", "conflist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Routes deleted behind the daemon's back come back on the next reconcile, and
// there is one on a timer even when no Node changes (ADR 0006).
func TestReconcileAsksToRunAgainPeriodically(t *testing.T) {
	r, _ := newReconciler(t, &recorder{}, node("node-a", []string{"192.0.2.0/24"}, "203.0.113.1"))
	res, err := r.Reconcile(t.Context(), ctrl.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %s, want a positive interval", res.RequeueAfter)
	}
}

// A peer with a pod CIDR of a family it has no InternalIP for gets a warning that
// names the node and the family, on every reconcile (ADR 0006).
func TestReconcileLogsAMissingFamily(t *testing.T) {
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := log.IntoContext(t.Context(), logger)

	r, _ := newReconciler(t, &recorder{},
		node("node-a", []string{"192.0.2.0/24", "2001:db8:a::/64"}, "203.0.113.1", "2001:db8::1"),
		node("node-b", []string{"198.51.100.0/24", "2001:db8:b::/64"}, "203.0.113.2"),
	)
	for range 2 {
		lines = nil
		if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "node-b") || !strings.Contains(joined, "IPv6") {
			t.Errorf("no warning naming node-b and IPv6 in:\n%s", joined)
		}
	}
}

// The uplink MTU is read from the interface that holds one of the node's addresses.
// Loopback is the one interface every test machine has.
func TestUplinkMTUReadsTheInterfaceHoldingTheAddress(t *testing.T) {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skip("no lo interface")
	}
	got, err := uplinkMTU([]netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	if got != lo.MTU {
		t.Errorf("uplinkMTU = %d, want lo's %d", got, lo.MTU)
	}
}

func TestUplinkMTUFailsWhenNoInterfaceHoldsTheAddress(t *testing.T) {
	if _, err := uplinkMTU([]netip.Addr{netip.MustParseAddr("192.0.2.1")}); err == nil {
		t.Error("uplinkMTU returned nil for an address no interface holds")
	}
}

// Every node's pod CIDRs, this node's included, go to the iptables forward chain, so
// that pod traffic survives a FORWARD policy of DROP (ADR 0003).
func TestReconcileHandsTheClusterPodCIDRsToTheForwardChain(t *testing.T) {
	forward := &forwardRecorder{}
	r, _ := newReconcilerWithForward(t, &recorder{}, forward,
		node("node-a", []string{"192.0.2.0/24", "2001:db8:a::/64"}, "203.0.113.1"),
		node("node-b", []string{"198.51.100.0/24"}, "203.0.113.2"),
	)
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:a::/64"),
		netip.MustParsePrefix("198.51.100.0/24"),
	}
	if diff := cmp.Diff(want, forward.got, cmp.Comparer(func(a, b netip.Prefix) bool { return a == b })); diff != "" {
		t.Errorf("CIDRs handed to the forward chain differ (-want +got):\n%s", diff)
	}
}

// A forward chain that could not be written comes back as an error for the queue to
// retry, and the routes are still applied in the same pass.
func TestReconcileReturnsForwardChainFailures(t *testing.T) {
	kernel := &recorder{}
	forward := &forwardRecorder{fail: errors.New("iptables-nft-restore: exit status 4")}
	r, _ := newReconcilerWithForward(t, kernel, forward,
		node("node-a", []string{"192.0.2.0/24"}, "203.0.113.1"),
		node("node-b", []string{"198.51.100.0/24"}, "203.0.113.2"),
	)
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	if err == nil || !strings.Contains(err.Error(), "iptables-nft-restore") {
		t.Errorf("Reconcile returned %v, want the forward chain's error", err)
	}
	if len(kernel.got) != 1 {
		t.Errorf("kernel got %v, want the route to node-b despite the failure", kernel.got)
	}
}
