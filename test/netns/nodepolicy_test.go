//go:build netns

package netns

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yuanying/cnidaria-cni/internal/apis/v1alpha1"
	"github.com/yuanying/cnidaria-cni/internal/nftables"
	"github.com/yuanying/cnidaria-cni/internal/nodepol"
	"github.com/yuanying/cnidaria-cni/test/netns/testbed"
)

// The node under policy listens on all three ports throughout, the one no policy
// allows included: a connection to a port nobody listens on is refused at once, which
// is not what a dropped packet looks like, and testbed.Outcome holds the two apart
// (ADR 0008).
const (
	allowedPort uint16 = 8080
	blockedPort uint16 = 9090
	kubeletPort uint16 = 10250 // a safe port, open whatever the policy says
)

// segmentPrefixes are the peers the policies name: the segment the nodes sit on.
var segmentPrefixes = []string{"203.0.113.0/24", "2001:db8::/64"}

// policyBed is a node with a policy on it and a neighbour to knock on its door with.
// Neither is a pod: what a NodePolicy governs is the node's own sockets.
type policyBed struct {
	node, peer *testbed.Node
}

func newPolicyBed(t *testing.T) policyBed {
	t.Helper()
	seg := testbed.NewSegment(t)
	bed := policyBed{
		node: seg.AddNode(t, testbed.NodeSpec{
			Name:        "a",
			PodCIDRs:    []netip.Prefix{pfx("192.0.2.0/25"), pfx("2001:db8:a::/64")},
			InternalIPs: []netip.Prefix{pfx("203.0.113.1/24"), pfx("2001:db8::1/64")},
		}),
		peer: seg.AddNode(t, testbed.NodeSpec{
			Name:        "b",
			PodCIDRs:    []netip.Prefix{pfx("192.0.2.128/25"), pfx("2001:db8:b::/64")},
			InternalIPs: []netip.Prefix{pfx("203.0.113.2/24"), pfx("2001:db8::2/64")},
		}),
	}
	for _, n := range []*testbed.Node{bed.node, bed.peer} {
		for _, port := range []uint16{allowedPort, blockedPort, kubeletPort} {
			n.Serve(t, port)
		}
	}
	return bed
}

// apply renders the table this node would carry with those policies and applies it,
// the way the daemon's reconciler does: the skeleton from nftables, the policies from
// nodepol, one nft -f. The daemon itself is not in the loop (ADR 0008).
func (b policyBed) apply(t *testing.T, policies ...v1alpha1.NodePolicy) map[string]v1alpha1.Mode {
	t.Helper()
	ruleset, err := nftables.Render(nftables.Params{
		PodCIDRs:        b.node.PodCIDRs,
		ClusterPodCIDRs: append(append([]netip.Prefix{}, b.node.PodCIDRs...), b.peer.PodCIDRs...),
		SafePorts:       nftables.DefaultSafePorts,
	})
	if err != nil {
		t.Fatalf("nftables.Render: %v", err)
	}
	modes, err := nodepol.Add(ruleset, nodepol.Node{Name: b.node.Name}, policies)
	if err != nil {
		t.Fatalf("nodepol.Add: %v", err)
	}
	b.node.ApplyRuleset(t, ruleset.String())
	return modes
}

// addrs of the node under policy, one per family.
func (b policyBed) addrs() []netip.Addr { return b.node.InternalIPs }

// The node accepts what the policy lists and what the safe rules keep open, and
// nothing else, while the policy enforces.
func TestEnforcedIngressDropsWhatThePolicyDoesNotList(t *testing.T) {
	bed := newPolicyBed(t)
	modes := bed.apply(t, ingressPolicy("segment", v1alpha1.ModeEnforce))
	if modes["segment"] != v1alpha1.ModeEnforce {
		t.Fatalf("mode of the applied policy = %q, want %q", modes["segment"], v1alpha1.ModeEnforce)
	}

	for _, addr := range bed.addrs() {
		fam := family(addr)
		// The reachable check comes first: it is what says the segment has
		// settled, so that a later failure is the policy and not a neighbour
		// the node has not found yet.
		testbed.Eventually(t, settle, fam+" to the port the policy lists",
			reaches(t, bed.peer, addr, allowedPort))
		testbed.Eventually(t, settle, fam+" to the kubelet port the safe rules keep open",
			reaches(t, bed.peer, addr, kubeletPort))
		if got := bed.peer.Connect(t, addr, blockedPort, connectTimeout); got != testbed.Dropped {
			t.Errorf("%s to the port the policy does not list, with the policy enforcing: %s, want %s",
				fam, got, testbed.Dropped)
		}
		// ICMP is a safe rule: a node that cannot be pinged has lost path MTU
		// discovery and neighbour discovery with it (ADR 0004).
		testbed.Eventually(t, settle, fam+" echo to a node that is enforcing", func() error {
			return ping(bed.peer, addr)
		})
	}

	// Loopback is a safe rule, so the node still reaches its own blocked port.
	for _, addr := range []netip.Addr{addr("127.0.0.1"), addr("::1")} {
		if got := bed.node.Connect(t, addr, blockedPort, connectTimeout); got != testbed.Open {
			t.Errorf("the node lost its own loopback while enforcing: %s to %s", got, addr)
		}
	}
}

// The same policy in the mode it is created in drops nothing, and says how much it
// would have dropped.
func TestPermissiveIngressCountsWhatEnforceWouldDrop(t *testing.T) {
	bed := newPolicyBed(t)
	modes := bed.apply(t, ingressPolicy("segment", v1alpha1.ModePermissive))
	if modes["segment"] != v1alpha1.ModePermissive {
		t.Fatalf("mode of the applied policy = %q, want %q", modes["segment"], v1alpha1.ModePermissive)
	}

	before := counted(t, bed.node, nodepol.ChainInputDispatch)
	for _, addr := range bed.addrs() {
		testbed.Eventually(t, settle, family(addr)+" to the port the policy does not list, under Permissive",
			reaches(t, bed.peer, addr, blockedPort))
	}
	// Whether the log line reaches the container's kernel log is not something the
	// test can insist on, but the counter is exact (ADR 0004).
	if after := counted(t, bed.node, nodepol.ChainInputDispatch); after <= before {
		t.Errorf("the permissive counter went from %d to %d: nothing was counted", before, after)
	}
}

// Egress is the same the other way round: what the node itself may open.
func TestEnforcedEgressDropsWhatThePolicyDoesNotList(t *testing.T) {
	bed := newPolicyBed(t)
	policy := ingressPolicy("upstream", v1alpha1.ModeEnforce)
	policy.Spec.PolicyTypes = []v1alpha1.PolicyType{v1alpha1.PolicyTypeEgress}
	policy.Spec.Egress = []v1alpha1.NodePolicyEgressRule{{
		To:    peers(),
		Ports: []v1alpha1.NodePolicyPort{{Protocol: corev1.ProtocolTCP, Port: ptr(int32(allowedPort))}},
	}}
	policy.Spec.Ingress = nil
	bed.apply(t, policy)

	for _, addr := range bed.peer.InternalIPs {
		fam := family(addr)
		testbed.Eventually(t, settle, fam+" out to the port the policy lists",
			reaches(t, bed.node, addr, allowedPort))
		if got := bed.node.Connect(t, addr, blockedPort, connectTimeout); got != testbed.Dropped {
			t.Errorf("%s out to the port the policy does not list, with the policy enforcing: %s, want %s",
				fam, got, testbed.Dropped)
		}
		// The neighbour is under no policy and still accepts everything.
		if got := bed.peer.Connect(t, addr, blockedPort, connectTimeout); got != testbed.Open {
			t.Errorf("a node under no policy answered %s on its own address", got)
		}
	}
}

// ingressPolicy allows the segment on one port and nothing else.
func ingressPolicy(name string, mode v1alpha1.Mode) v1alpha1.NodePolicy {
	return v1alpha1.NodePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.NodePolicySpec{
			Mode: mode,
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				From:  peers(),
				Ports: []v1alpha1.NodePolicyPort{{Protocol: corev1.ProtocolTCP, Port: ptr(int32(allowedPort))}},
			}},
		},
	}
}

func peers() []v1alpha1.NodePolicyPeer {
	out := make([]v1alpha1.NodePolicyPeer, 0, len(segmentPrefixes))
	for _, cidr := range segmentPrefixes {
		out = append(out, v1alpha1.NodePolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func family(a netip.Addr) string {
	if a.Is6() {
		return "IPv6"
	}
	return "IPv4"
}

// reaches is the probe for a connection that has to get through. Anything else,
// dropped or refused, is reported as it happened.
func reaches(t *testing.T, from *testbed.Node, to netip.Addr, port uint16) func() error {
	t.Helper()
	return func() error {
		if got := from.Connect(t, to, port, connectTimeout); got != testbed.Open {
			return fmt.Errorf("connection from node %s to %s port %d: %s", from.Name, to, port, got)
		}
		return nil
	}
}

func ping(from *testbed.Node, to netip.Addr) error {
	out, err := from.Try("ping", "-c", "1", "-W", "1", to.String())
	if err != nil {
		return fmt.Errorf("ping %s from %s: %w\n%s", to, from.Name, err, strings.TrimSpace(out))
	}
	return nil
}

// This is the comment nodepol puts on the rule that counts, without a rate limit,
// what Enforce would have dropped.
const permissiveCounterComment = "nodepolicy: permissive, counted and accepted"

var packetsCounted = regexp.MustCompile(`counter packets (\d+)`)

// counted reads the packet count off the permissive rule at the end of a dispatch
// chain.
func counted(t *testing.T, n *testbed.Node, chain string) int {
	t.Helper()
	out, err := n.Try("nft", "list", "chain", nftables.TableFamily, nftables.TableName, chain)
	if err != nil {
		t.Fatalf("listing chain %s on node %s: %v\n%s", chain, n.Name, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, permissiveCounterComment) {
			continue
		}
		m := packetsCounted.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("the permissive rule carries no counter: %s", line)
		}
		count, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	t.Fatalf("chain %s has no rule commented %q:\n%s", chain, permissiveCounterComment, out)
	return 0
}
