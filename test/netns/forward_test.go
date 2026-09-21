//go:build netns

package netns

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/yuanying/cnidaria-cni/test/netns/testbed"
)

// A container engine or a host firewall may set the policy of iptables' FORWARD to
// DROP. cnidaria's nftables table cannot override that, so the daemon keeps a chain
// of its own that accepts pod traffic, with a jump to it first in FORWARD (ADR 0003).
// Pods that reach each other with the policy at ACCEPT stop reaching each other at
// DROP, and reach each other again once the chain is in place.
func TestPodTrafficPassesAForwardPolicyOfDrop(t *testing.T) {
	cl := newCluster(t)
	a1 := cl.a.AddPod(t, "a1")
	a2 := cl.a.AddPod(t, "a2")
	b1 := cl.b.AddPod(t, "b1")
	for _, n := range []*testbed.Node{cl.a, cl.b} {
		if _, err := cl.apply(t, n); err != nil {
			t.Fatalf("routes on node %s: %v", n.Name, err)
		}
	}
	fam := map[bool]string{false: "IPv4", true: "IPv6"}
	for _, v6 := range []bool{false, true} {
		testbed.Eventually(t, settle, fam[v6]+" between two pods on node a", func() error { return a1.Ping(a2.IP(t, v6)) })
		testbed.Eventually(t, settle, fam[v6]+" from node a to node b", func() error { return a1.Ping(b1.IP(t, v6)) })
	}

	for _, n := range []*testbed.Node{cl.a, cl.b} {
		n.Exec(t, "iptables-nft", "-P", "FORWARD", "DROP")
		n.Exec(t, "ip6tables-nft", "-P", "FORWARD", "DROP")
	}
	for _, v6 := range []bool{false, true} {
		if err := a1.Ping(a2.IP(t, v6)); err == nil {
			t.Errorf("%s between two pods on node a got through a FORWARD policy of DROP", fam[v6])
		}
		if err := a1.Ping(b1.IP(t, v6)); err == nil {
			t.Errorf("%s from node a to node b got through a FORWARD policy of DROP", fam[v6])
		}
	}

	var cidrs []netip.Prefix
	for _, n := range []*testbed.Node{cl.a, cl.b, cl.c} {
		cidrs = append(cidrs, n.PodCIDRs...)
	}
	for _, n := range []*testbed.Node{cl.a, cl.b} {
		f := n.Forward(t)
		// Twice: the second run must find the jump and not add another, and
		// must replace the chain's rules rather than add to them.
		for range 2 {
			if err := f.Apply(t.Context(), cidrs); err != nil {
				t.Fatalf("forward chain on node %s: %v", n.Name, err)
			}
		}
	}
	for _, prog := range []string{"iptables-nft", "ip6tables-nft"} {
		rules := cl.a.Exec(t, prog, "-S", "CNIDARIA-FWD")
		// One rule from and one to each of the three nodes' CIDRs of the family.
		if got := strings.Count(rules, "-A CNIDARIA-FWD "); got != 2*3 {
			t.Errorf("%s CNIDARIA-FWD on node a has %d rules after two applies, want %d:\n%s", prog, got, 2*3, rules)
		}
	}
	// IPv6 neighbour discovery between two pods on one bridge goes through
	// ip6tables' FORWARD as well (ADR 0002); ARP is not IP and does not. The
	// caches are emptied so that discovery is shown to get through, not answered
	// from what the pings at ACCEPT left behind.
	for _, p := range []*testbed.Pod{a1, a2, b1} {
		p.Exec(t, "ip", "neigh", "flush", "all")
	}
	for _, v6 := range []bool{false, true} {
		testbed.Eventually(t, settle, fam[v6]+" between two pods on node a past DROP", func() error { return a1.Ping(a2.IP(t, v6)) })
		testbed.Eventually(t, settle, fam[v6]+" from node a to node b past DROP", func() error { return a1.Ping(b1.IP(t, v6)) })
		testbed.Eventually(t, settle, fam[v6]+" from node b to node a past DROP", func() error { return b1.Ping(a1.IP(t, v6)) })
	}

	for _, prog := range []string{"iptables-nft", "ip6tables-nft"} {
		rules := cl.a.Exec(t, prog, "-S", "FORWARD")
		if got := strings.Count(rules, "-j CNIDARIA-FWD"); got != 1 {
			t.Errorf("%s FORWARD on node a has %d jumps to CNIDARIA-FWD, want 1:\n%s", prog, got, rules)
		}
		if !strings.Contains(rules, "-P FORWARD DROP") {
			t.Errorf("%s FORWARD policy on node a was changed:\n%s", prog, rules)
		}
	}
}
