//go:build netns

package netns

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yuanying/cnidaria-cni/internal/netpol"
	"github.com/yuanying/cnidaria-cni/internal/nftables"
	"github.com/yuanying/cnidaria-cni/internal/routes"
	"github.com/yuanying/cnidaria-cni/test/netns/testbed"
)

// The ports every pod serves, and the name it gives the first of them. A policy that
// names the port by name has to reach the same one.
const (
	servedPort   uint16 = 8080
	otherPort    uint16 = 9090
	unservedPort uint16 = 9999
	servedName          = "serve"
)

// A connection that a rule dropped produces nothing at all and runs out of time,
// while a port nobody listens on answers at once. This is the wait that tells the two
// apart (ADR 0008).
const connectTimeout = 2 * time.Second

// policyCluster is two nodes on one segment, each with pods, the host-gw routes in
// place and every pod serving. The daemon is not in the loop: each node has the table
// its renderers produce applied to it directly (ADR 0008).
type policyCluster struct {
	nodes        []*testbed.Node
	pods         map[string]*testbed.Pod
	specs        []netpol.Pod
	namespaces   []netpol.Namespace
	clusterCIDRs []netip.Prefix
}

func newPolicyCluster(t *testing.T) *policyCluster {
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
	all := []routes.Node{a.RoutesNode(), b.RoutesNode()}
	for _, n := range []*testbed.Node{a, b} {
		set, _ := routes.Compute(n.Name, all)
		if err := n.Kernel(t).Apply(set); err != nil {
			t.Fatalf("routes on node %s: %v", n.Name, err)
		}
	}

	c := &policyCluster{
		nodes: []*testbed.Node{a, b},
		pods:  map[string]*testbed.Pod{},
		namespaces: []netpol.Namespace{
			{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}},
			{Name: "other", Labels: map[string]string{"kubernetes.io/metadata.name": "other", "tier": "trusted"}},
		},
		clusterCIDRs: append(append([]netip.Prefix{}, a.PodCIDRs...), b.PodCIDRs...),
	}
	// web is what the policies protect. cli and far are clients of it, on this node
	// and on the other one; probe is a client in another namespace. Only cli carries
	// role=front, so a policy can name it without naming far.
	c.addPod(t, a, "default", "web", map[string]string{"app": "web"})
	c.addPod(t, a, "default", "cli", map[string]string{"app": "client", "role": "front"})
	c.addPod(t, b, "default", "far", map[string]string{"app": "client"})
	c.addPod(t, b, "other", "probe", map[string]string{"app": "probe"})
	c.warmUp(t)
	return c
}

func (c *policyCluster) addPod(t *testing.T, n *testbed.Node, namespace, name string, labels map[string]string) {
	t.Helper()
	p := n.AddPod(t, name)
	p.Serve(t, servedPort)
	p.Serve(t, otherPort)
	c.pods[namespace+"/"+name] = p
	c.specs = append(c.specs, netpol.Pod{
		Namespace: namespace, Name: name, NodeName: n.Name, Labels: labels, IPs: p.IPs,
		Ports: []netpol.NamedPort{{Name: servedName, Port: servedPort, Protocol: netpol.ProtocolTCP}},
	})
}

func (c *policyCluster) pod(t *testing.T, key string) *testbed.Pod {
	t.Helper()
	p, ok := c.pods[key]
	if !ok {
		t.Fatalf("no pod %s in the cluster", key)
	}
	return p
}

// warmUp settles addresses and fills the neighbour caches before any policy is in
// place, so that a later "allowed" is not waiting on discovery.
func (c *policyCluster) warmUp(t *testing.T) {
	t.Helper()
	for _, v6 := range []bool{false, true} {
		for from := range c.pods {
			for to := range c.pods {
				if from == to {
					continue
				}
				from, to := c.pod(t, from), c.pod(t, to)
				testbed.Eventually(t, settle, fmt.Sprintf("%s reaching %s before any policy", from.Name, to.Name),
					func() error { return from.Ping(to.IP(t, v6)) })
			}
		}
	}
}

// enforce renders each node's table with the policies and applies it to that node,
// which is what the daemon on it would do.
func (c *policyCluster) enforce(t *testing.T, policies ...netpol.NetworkPolicy) {
	t.Helper()
	for _, n := range c.nodes {
		r, err := nftables.Render(nftables.Params{
			PodCIDRs:        n.PodCIDRs,
			ClusterPodCIDRs: c.clusterCIDRs,
			SafePorts:       nftables.DefaultSafePorts,
		})
		if err != nil {
			t.Fatalf("rendering the table for node %s: %v", n.Name, err)
		}
		if err := netpol.Add(r, netpol.Params{
			NodeName: n.Name, Pods: c.specs, Namespaces: c.namespaces, Policies: policies,
		}); err != nil {
			t.Fatalf("adding the policies for node %s: %v", n.Name, err)
		}
		n.ApplyRuleset(t, r.String())
	}
}

// reach is one expectation: a pod, a pod it connects to, a port, and how that should
// end.
type reach struct {
	from, to string
	port     uint16
	want     testbed.Outcome
}

// check runs every expectation over both address families, which is where a policy
// that only works for one of them shows up.
func (c *policyCluster) check(t *testing.T, cases ...reach) {
	t.Helper()
	for _, v6 := range []bool{false, true} {
		family := "IPv4"
		if v6 {
			family = "IPv6"
		}
		for _, tc := range cases {
			what := fmt.Sprintf("%s from %s to %s:%d", family, tc.from, tc.to, tc.port)
			from, to := c.pod(t, tc.from), c.pod(t, tc.to)
			target := to.IP(t, v6)
			if tc.want == testbed.Open {
				testbed.Eventually(t, settle, what, func() error {
					if got := from.Connect(t, target, tc.port, connectTimeout); got != testbed.Open {
						return fmt.Errorf("the connection was %s", got)
					}
					return nil
				})
				continue
			}
			if got := from.Connect(t, target, tc.port, connectTimeout); got != tc.want {
				t.Errorf("%s: the connection was %s, want %s", what, got, tc.want)
			}
		}
	}
}

func selector(kv ...string) metav1.LabelSelector {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return metav1.LabelSelector{MatchLabels: m}
}

func selectorPtr(kv ...string) *metav1.LabelSelector {
	s := selector(kv...)
	return &s
}

// A table with no policy in it leaves every pod reachable, and a port nobody listens
// on still answers with a reset. That reset is what a dropped packet is told apart
// from below.
func TestWithoutAPolicyEveryPodTakesEverything(t *testing.T) {
	c := newPolicyCluster(t)
	c.enforce(t)
	c.check(t,
		reach{"default/cli", "default/web", servedPort, testbed.Open},
		reach{"other/probe", "default/web", servedPort, testbed.Open},
		reach{"default/web", "default/far", otherPort, testbed.Open},
		reach{"default/cli", "default/web", unservedPort, testbed.Refused},
	)
}

// An ingress policy isolates the pods it selects: what it lists is allowed, from this
// node and from the other one alike, and everything else is dropped, including a port
// that something is listening on.
func TestAnIngressPolicyAllowsOnlyWhatItLists(t *testing.T) {
	c := newPolicyCluster(t)
	c.enforce(t, netpol.NetworkPolicy{
		Namespace: "default", Name: "web-in", PodSelector: selector("app", "web"),
		Ingress: []netpol.Rule{{
			Peers: []netpol.Peer{{PodSelector: selectorPtr("app", "client")}},
			Ports: []netpol.Port{{Protocol: netpol.ProtocolTCP, Number: servedPort}},
		}},
	})
	c.check(t,
		reach{"default/cli", "default/web", servedPort, testbed.Open},
		reach{"default/far", "default/web", servedPort, testbed.Open},
		reach{"other/probe", "default/web", servedPort, testbed.Dropped},
		reach{"default/cli", "default/web", otherPort, testbed.Dropped},
		// Only web is isolated, and only for ingress.
		reach{"default/web", "default/cli", servedPort, testbed.Open},
		reach{"other/probe", "default/far", servedPort, testbed.Open},
	)
}

// A peer that names namespaces takes every pod of them and nothing outside them.
func TestANamespaceSelectorTakesAWholeNamespace(t *testing.T) {
	c := newPolicyCluster(t)
	c.enforce(t, netpol.NetworkPolicy{
		Namespace: "default", Name: "web-in", PodSelector: selector("app", "web"),
		Ingress: []netpol.Rule{{Peers: []netpol.Peer{{NamespaceSelector: selectorPtr("tier", "trusted")}}}},
	})
	c.check(t,
		reach{"other/probe", "default/web", servedPort, testbed.Open},
		reach{"default/cli", "default/web", servedPort, testbed.Dropped},
		reach{"default/far", "default/web", servedPort, testbed.Dropped},
	)
}

// An ipBlock allows a range of addresses and its except list is cut out of it, so a
// pod inside the range but inside an except is denied like one outside the range.
func TestAnIPBlockExceptIsCutOut(t *testing.T) {
	c := newPolicyCluster(t)
	far := c.pod(t, "default/far")
	nodeB := c.nodes[1]
	var peers []netpol.Peer
	for _, v6 := range []bool{false, true} {
		cidr := nodeB.PodCIDRs[0]
		if v6 {
			cidr = nodeB.PodCIDRs[1]
		}
		peers = append(peers, netpol.Peer{IPBlock: &netpol.IPBlock{
			CIDR:   cidr,
			Except: []netip.Prefix{netip.PrefixFrom(far.IP(t, v6), far.IP(t, v6).BitLen())},
		}})
	}
	c.enforce(t, netpol.NetworkPolicy{
		Namespace: "default", Name: "web-in", PodSelector: selector("app", "web"),
		Ingress: []netpol.Rule{{Peers: peers}},
	})
	c.check(t,
		reach{"other/probe", "default/web", servedPort, testbed.Open},
		reach{"default/far", "default/web", servedPort, testbed.Dropped},
		reach{"default/cli", "default/web", servedPort, testbed.Dropped},
	)
}

// policyTypes Egress isolates what leaves the selected pods and leaves what arrives
// at them alone. The port here is named, so it is resolved from the container ports
// of the pods the entry names.
func TestAnEgressPolicyGovernsOnlyWhatLeaves(t *testing.T) {
	c := newPolicyCluster(t)
	c.enforce(t, netpol.NetworkPolicy{
		Namespace: "default", Name: "cli-out", PodSelector: selector("role", "front"),
		PolicyTypes: []netpol.PolicyType{netpol.PolicyTypeEgress},
		Egress: []netpol.Rule{{
			Peers: []netpol.Peer{{PodSelector: selectorPtr("app", "web")}},
			Ports: []netpol.Port{{Name: servedName}},
		}},
	})
	c.check(t,
		reach{"default/cli", "default/web", servedPort, testbed.Open},
		reach{"default/cli", "default/web", otherPort, testbed.Dropped},
		reach{"default/cli", "other/probe", servedPort, testbed.Dropped},
		// Nothing about what reaches cli changed, and far was never selected.
		reach{"default/far", "default/cli", servedPort, testbed.Open},
		reach{"default/far", "other/probe", servedPort, testbed.Open},
	)
}

// A policy that declares both directions and no rules at all denies both, which is
// how a deny-all is written.
func TestAPolicyWithNoRulesDeniesBothDirections(t *testing.T) {
	c := newPolicyCluster(t)
	c.enforce(t, netpol.NetworkPolicy{
		Namespace: "default", Name: "web-deny", PodSelector: selector("app", "web"),
		PolicyTypes: []netpol.PolicyType{netpol.PolicyTypeIngress, netpol.PolicyTypeEgress},
	})
	c.check(t,
		reach{"default/cli", "default/web", servedPort, testbed.Dropped},
		reach{"default/far", "default/web", servedPort, testbed.Dropped},
		reach{"default/web", "default/cli", servedPort, testbed.Dropped},
		reach{"default/web", "default/far", servedPort, testbed.Dropped},
		reach{"default/cli", "default/far", servedPort, testbed.Open},
	)
}

// Two policies on one pod are an OR: a packet gets through if either accepts it.
func TestTwoPoliciesOnOnePodAreAnOr(t *testing.T) {
	c := newPolicyCluster(t)
	c.enforce(t,
		netpol.NetworkPolicy{
			Namespace: "default", Name: "from-front", PodSelector: selector("app", "web"),
			Ingress: []netpol.Rule{{Peers: []netpol.Peer{{PodSelector: selectorPtr("role", "front")}}}},
		},
		netpol.NetworkPolicy{
			Namespace: "default", Name: "from-other", PodSelector: selector("app", "web"),
			Ingress: []netpol.Rule{{Peers: []netpol.Peer{{NamespaceSelector: selectorPtr("tier", "trusted")}}}},
		},
	)
	c.check(t,
		reach{"default/cli", "default/web", servedPort, testbed.Open},
		reach{"other/probe", "default/web", servedPort, testbed.Open},
		reach{"default/far", "default/web", servedPort, testbed.Dropped},
	)
}
