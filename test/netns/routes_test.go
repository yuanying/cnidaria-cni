//go:build netns

package netns

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/yuanying/cnidaria-cni/internal/routes"
	"github.com/yuanying/cnidaria-cni/test/netns/testbed"
)

const settle = 15 * time.Second

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr  { return netip.MustParseAddr(s) }

// cluster is three nodes on one segment. Node c has an IPv6 pod CIDR but only an
// IPv4 InternalIP, which is the half-connected case ADR 0006 describes.
type cluster struct {
	a, b, c *testbed.Node
	all     []routes.Node
}

func newCluster(t *testing.T) cluster {
	t.Helper()
	seg := testbed.NewSegment(t)
	a := seg.AddNode(t, testbed.NodeSpec{
		Name:        "a",
		PodCIDRs:    []netip.Prefix{pfx("192.0.2.0/25"), pfx("2001:db8:a::/64")},
		InternalIPs: []netip.Prefix{pfx("203.0.113.1/24"), pfx("2001:db8::1/64")},
	})
	b := seg.AddNode(t, testbed.NodeSpec{
		Name:        "b",
		PodCIDRs:    []netip.Prefix{pfx("192.0.2.128/25"), pfx("2001:db8:b::/64")},
		InternalIPs: []netip.Prefix{pfx("203.0.113.2/24"), pfx("2001:db8::2/64")},
	})
	c := seg.AddNode(t, testbed.NodeSpec{
		Name:        "c",
		PodCIDRs:    []netip.Prefix{pfx("198.51.100.0/25"), pfx("2001:db8:c::/64")},
		InternalIPs: []netip.Prefix{pfx("203.0.113.3/24")},
	})
	return cluster{a: a, b: b, c: c, all: []routes.Node{a.RoutesNode(), b.RoutesNode(), c.RoutesNode()}}
}

// apply computes and installs the route set of one node, as the daemon on that
// node would, and returns what it could not route and what failed to install.
func (c cluster) apply(t *testing.T, n *testbed.Node) ([]routes.Missing, error) {
	t.Helper()
	set, missing := routes.Compute(n.Name, c.all)
	return missing, n.Kernel(t).Apply(set)
}

func TestPodsReachEachOtherOnOneNodeAndAcrossNodes(t *testing.T) {
	cl := newCluster(t)
	a1 := cl.a.AddPod(t, "a1")
	a2 := cl.a.AddPod(t, "a2")
	b1 := cl.b.AddPod(t, "b1")
	c1 := cl.c.AddPod(t, "c1")

	for _, n := range []*testbed.Node{cl.a, cl.b} {
		if _, err := cl.apply(t, n); err != nil {
			t.Fatalf("routes on node %s: %v", n.Name, err)
		}
	}
	// Node c has no IPv6 address on the segment, so the IPv6 routes to a and b have
	// gateways that are not on-link. Each failure names its node; the IPv4 routes
	// go in regardless (ADR 0006).
	_, err := cl.apply(t, cl.c)
	if err == nil || !strings.Contains(err.Error(), "node a") || !strings.Contains(err.Error(), "node b") {
		t.Fatalf("routes on node c: got %v, want failures naming node a and node b", err)
	}

	for _, v6 := range []bool{false, true} {
		fam := map[bool]string{false: "IPv4", true: "IPv6"}[v6]
		testbed.Eventually(t, settle, fam+" between two pods on node a", func() error { return a1.Ping(a2.IP(t, v6)) })
		testbed.Eventually(t, settle, fam+" from a pod on node a to a pod on node b", func() error { return a1.Ping(b1.IP(t, v6)) })
		testbed.Eventually(t, settle, fam+" from a pod on node b to a pod on node a", func() error { return b1.Ping(a1.IP(t, v6)) })
	}
	testbed.Eventually(t, settle, "IPv4 from a pod on node a to a pod on node c", func() error { return a1.Ping(c1.IP(t, false)) })
	testbed.Eventually(t, settle, "IPv4 from a pod on node c to a pod on node b", func() error { return c1.Ping(b1.IP(t, false)) })
}

// A peer with an IPv6 pod CIDR but no IPv6 InternalIP gets no IPv6 route and a
// warning that names it. Its IPv4 side keeps working (ADR 0006).
func TestAMissingFamilyIsReportedAndNotWorkedAround(t *testing.T) {
	cl := newCluster(t)
	a1 := cl.a.AddPod(t, "a1")
	c1 := cl.c.AddPod(t, "c1")

	missing, err := cl.apply(t, cl.a)
	if err != nil {
		t.Fatalf("routes on node a: %v", err)
	}
	want := []routes.Missing{{Node: "c", Family: "IPv6", PodCIDR: pfx("2001:db8:c::/64")}}
	if len(missing) != 1 || missing[0] != want[0] {
		t.Errorf("node a reported %v, want %v", missing, want)
	}
	if _, err := cl.apply(t, cl.c); err == nil {
		t.Fatal("routes on node c: got nil, want the IPv6 gateways reported as unreachable")
	}

	testbed.Eventually(t, settle, "IPv4 from node a to node c", func() error { return a1.Ping(c1.IP(t, false)) })
	if err := a1.Ping(c1.IP(t, true)); err == nil {
		t.Error("IPv6 from node a reached node c although no IPv6 route exists")
	}
}

// A node whose Node object has no IPv6 InternalIP still has a global IPv6 address on
// its uplink. It finds that address and publishes it, and its peers route its IPv6 pod
// CIDR through it (ADR 0006). The address here is what the node would put into its
// annotation; the reconciler's round trip through the API is covered by its own
// unit tests.
func TestAPeerIsRoutedOverIPv6ThroughTheAddressItPublishes(t *testing.T) {
	seg := testbed.NewSegment(t)
	a := seg.AddNode(t, testbed.NodeSpec{
		Name:        "a",
		PodCIDRs:    []netip.Prefix{pfx("192.0.2.0/25"), pfx("2001:db8:a::/64")},
		InternalIPs: []netip.Prefix{pfx("203.0.113.1/24"), pfx("2001:db8::1/64")},
	})
	c := seg.AddNode(t, testbed.NodeSpec{
		Name:        "c",
		PodCIDRs:    []netip.Prefix{pfx("198.51.100.0/25"), pfx("2001:db8:c::/64")},
		InternalIPs: []netip.Prefix{pfx("203.0.113.3/24")},
		OtherIPs:    []netip.Prefix{pfx("2001:db8::3/64")},
	})
	a1 := a.AddPod(t, "a1")
	c1 := c.AddPod(t, "c1")

	published, ok, err := c.Kernel(t).GlobalIPv6(c.InternalIPs[0])
	if err != nil || !ok || published != addr("2001:db8::3") {
		t.Fatalf("node c found %s (ok %v, err %v), want 2001:db8::3", published, ok, err)
	}
	peerC := c.RoutesNode()
	peerC.AnnotatedIPv6 = published
	all := []routes.Node{a.RoutesNode(), peerC}
	for _, n := range []*testbed.Node{a, c} {
		set, missing := routes.Compute(n.Name, all)
		if len(missing) != 0 {
			t.Errorf("node %s could not route %v", n.Name, missing)
		}
		if err := n.Kernel(t).Apply(set); err != nil {
			t.Fatalf("routes on node %s: %v", n.Name, err)
		}
	}
	if table := routeTable(t, a); !strings.Contains(table, "2001:db8:c::/64 via 2001:db8::3") {
		t.Errorf("node a has no IPv6 route to node c through its published address\n%s", table)
	}
	testbed.Eventually(t, settle, "IPv6 from a pod on node a to a pod on node c", func() error { return a1.Ping(c1.IP(t, true)) })
	testbed.Eventually(t, settle, "IPv6 from a pod on node c to a pod on node a", func() error { return c1.Ping(a1.IP(t, true)) })
}

// A pod that sends to a virtual address which the node DNATs back to the same pod
// must get its packet back through its own bridge port. With br_netfilter (ADR
// 0002) the DNATed frame stays on the bridge, so this only works with hairpin on
// that port, which the conflist asks the bridge plugin for (ADR 0001).
//
// The nat rules stand in for kube-proxy serving a Service whose endpoint is the
// client: the destination is rewritten to the pod, and the source is masqueraded so
// that the pod does not see its own address as the sender and the reply comes back
// through the node to be un-NATed.
func TestHairpinLetsAPodReachItselfThroughTheBridge(t *testing.T) {
	cl := newCluster(t)
	a1 := cl.a.AddPod(t, "a1")
	vip4, vip6 := addr("198.51.100.250"), addr("2001:db8:ffff::250")
	pod4, pod6 := a1.IP(t, false).String(), a1.IP(t, true).String()

	nft := func(args ...string) { cl.a.Exec(t, append([]string{"nft"}, args...)...) }
	nft("add", "table", "inet", "hairpin-test")
	nft("add", "chain", "inet", "hairpin-test", "pre", "{ type nat hook prerouting priority -100; }")
	nft("add", "chain", "inet", "hairpin-test", "post", "{ type nat hook postrouting priority 100; }")
	nft("add", "rule", "inet", "hairpin-test", "pre", "ip", "daddr", vip4.String(), "dnat", "ip", "to", pod4)
	nft("add", "rule", "inet", "hairpin-test", "pre", "ip6", "daddr", vip6.String(), "dnat", "ip6", "to", pod6)
	nft("add", "rule", "inet", "hairpin-test", "post", "ip", "saddr", pod4, "ip", "daddr", pod4, "masquerade")
	nft("add", "rule", "inet", "hairpin-test", "post", "ip6", "saddr", pod6, "ip6", "daddr", pod6, "masquerade")

	testbed.Eventually(t, settle, "IPv4 hairpin", func() error { return a1.Ping(vip4) })
	testbed.Eventually(t, settle, "IPv6 hairpin", func() error { return a1.Ping(vip6) })

	// Turning hairpin off on the port is what makes the same packet disappear, so
	// the check above is shown to hinge on hairpin and nothing else.
	cl.a.Exec(t, "bridge", "link", "set", "dev", a1.HostVeth, "hairpin", "off")
	if err := a1.Ping(vip4); err == nil {
		t.Error("IPv4 reached the pod through its own port with hairpin off")
	}
	if err := a1.Ping(vip6); err == nil {
		t.Error("IPv6 reached the pod through its own port with hairpin off")
	}
	cl.a.Exec(t, "bridge", "link", "set", "dev", a1.HostVeth, "hairpin", "on")
	testbed.Eventually(t, settle, "IPv4 hairpin once more", func() error { return a1.Ping(vip4) })
}

// Applying the same set twice changes nothing. Routes without cnidaria's protocol
// marker are never touched; a marked route that is no longer wanted is removed.
func TestApplyIsIdempotentAndOwnsOnlyItsOwnRoutes(t *testing.T) {
	cl := newCluster(t)
	a1 := cl.a.AddPod(t, "a1")
	b1 := cl.b.AddPod(t, "b1")
	if _, err := cl.apply(t, cl.b); err != nil {
		t.Fatal(err)
	}

	// Somebody else's routes, and a leftover of cnidaria's own from a node that is gone.
	cl.a.Exec(t, "ip", "route", "add", "198.51.100.128/25", "via", "203.0.113.2", "proto", "static")
	cl.a.Exec(t, "ip", "-6", "route", "add", "2001:db8:f::/64", "via", "2001:db8::2", "proto", "static")
	cl.a.Exec(t, "ip", "route", "add", "198.51.100.192/26", "via", "203.0.113.3", "proto", "200")

	if _, err := cl.apply(t, cl.a); err != nil {
		t.Fatal(err)
	}
	first := routeTable(t, cl.a)
	if _, err := cl.apply(t, cl.a); err != nil {
		t.Fatal(err)
	}
	second := routeTable(t, cl.a)
	if first != second {
		t.Errorf("the second apply changed the table\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	for _, keep := range []string{"198.51.100.128/25 via 203.0.113.2", "2001:db8:f::/64 via 2001:db8::2"} {
		if !strings.Contains(first, keep) {
			t.Errorf("a route that is not cnidaria's was removed: %s\n%s", keep, first)
		}
	}
	if strings.Contains(first, "198.51.100.192/26") {
		t.Errorf("a stale cnidaria route survived\n%s", first)
	}
	testbed.Eventually(t, settle, "IPv4 across nodes after two applies", func() error { return a1.Ping(b1.IP(t, false)) })
	testbed.Eventually(t, settle, "IPv6 across nodes after two applies", func() error { return b1.Ping(a1.IP(t, true)) })
}

func routeTable(t *testing.T, n *testbed.Node) string {
	t.Helper()
	return n.Exec(t, "ip", "-4", "route", "show") + n.Exec(t, "ip", "-6", "route", "show")
}
