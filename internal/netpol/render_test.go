package netpol

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yuanying/cnidaria-cni/internal/nftables"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the renderer produces")

func ips(s ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(s))
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

// sel is a selector on matchLabels, from key, value pairs.
func sel(kv ...string) metav1.LabelSelector {
	if len(kv)%2 != 0 {
		panic("sel wants pairs")
	}
	if len(kv) == 0 {
		return metav1.LabelSelector{}
	}
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return metav1.LabelSelector{MatchLabels: m}
}

func selp(kv ...string) *metav1.LabelSelector {
	s := sel(kv...)
	return &s
}

func pod(namespace, name, node string, labels map[string]string, addrs ...string) Pod {
	return Pod{Namespace: namespace, Name: name, NodeName: node, Labels: labels, IPs: ips(addrs...)}
}

// cluster is what the cases below draw on: two namespaces, two pods of this node and
// two of another, so that "only this node's pods are enforced here" is visible.
func cluster() Params {
	return Params{
		NodeName: "node-a",
		Namespaces: []Namespace{
			{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}},
			{Name: "other", Labels: map[string]string{"kubernetes.io/metadata.name": "other", "tier": "trusted"}},
		},
		Pods: []Pod{
			pod("default", "web", "node-a", map[string]string{"app": "web"}, "192.0.2.10", "2001:db8:1::10"),
			pod("default", "client", "node-a", map[string]string{"app": "client"}, "192.0.2.11", "2001:db8:1::11"),
			pod("default", "away", "node-b", map[string]string{"app": "web"}, "198.51.100.10", "2001:db8:2::10"),
			pod("other", "probe", "node-b", map[string]string{"app": "probe"}, "198.51.100.11", "2001:db8:2::11"),
		},
	}
}

// nodeParams is the skeleton the policies are added to: this node is node-a.
var nodeParams = nftables.Params{
	PodCIDRs:        []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:1::/64")},
	ClusterPodCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:1::/64"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("2001:db8:2::/64")},
	SafePorts:       nftables.DefaultSafePorts,
}

// build renders the node's table and adds the policies to it, as the reconciler does.
func build(t *testing.T, p Params) *nftables.Ruleset {
	t.Helper()
	r, err := nftables.Render(nodeParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := Add(r, p); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return r
}

// rulesOf is the chain's rules as the text they render to.
func rulesOf(t *testing.T, r *nftables.Ruleset, chain string) []string {
	t.Helper()
	c := r.Chain(chain)
	if c == nil {
		t.Fatalf("the table has no chain %s", chain)
	}
	out := make([]string, 0, len(c.Rules))
	for _, rule := range c.Rules {
		out = append(out, rule.String())
	}
	return out
}

func elementsOf(t *testing.T, r *nftables.Ruleset, set string) []string {
	t.Helper()
	s := r.Set(set)
	if s == nil {
		t.Fatalf("the table has no set %s", set)
	}
	return s.Elements
}

func wantLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n--- got ---\n%s\n--- want ---\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A pod no policy selects is in no isolated set, so the base chains never send it to
// a dispatch chain and it is allowed everything (ADR 0003).
func TestAPodNoPolicySelectsIsNotIsolated(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "web-in", PodSelector: sel("app", "web"),
		Ingress: []Rule{{Peers: []Peer{{PodSelector: selp("app", "client")}}}},
	}}
	r := build(t, p)

	wantLines(t, "isolated_ingress_v4", elementsOf(t, r, nftables.SetIsolatedIngressV4), []string{"192.0.2.10"})
	wantLines(t, "isolated_ingress_v6", elementsOf(t, r, nftables.SetIsolatedIngressV6), []string{"2001:db8:1::10"})
	wantLines(t, "isolated_egress_v4", elementsOf(t, r, nftables.SetIsolatedEgressV4), nil)
	if r.Chain("egress_default/web-in") != nil {
		t.Error("a policy that governs ingress only got an egress chain")
	}
}

// The dispatch chain sends a packet to the chain of every policy whose selected pods
// hold the address, and drops it at the end if none accepted (ADR 0003).
func TestDispatchJumpsComeBeforeTheDrop(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{
		{Namespace: "default", Name: "b-in", PodSelector: sel("app", "web"), Ingress: []Rule{{}}},
		{Namespace: "default", Name: "a-in", PodSelector: sel("app", "client"), Ingress: []Rule{{}}},
	}
	r := build(t, p)

	wantLines(t, "ingress_dispatch", rulesOf(t, r, nftables.ChainIngressDispatch), []string{
		`ip daddr @pods_default/a-in/v4 counter jump ingress_default/a-in comment "default/a-in"`,
		`ip6 daddr @pods_default/a-in/v6 counter jump ingress_default/a-in comment "default/a-in"`,
		`ip daddr @pods_default/b-in/v4 counter jump ingress_default/b-in comment "default/b-in"`,
		`ip6 daddr @pods_default/b-in/v6 counter jump ingress_default/b-in comment "default/b-in"`,
		`counter drop comment "isolated pod, no policy accepted"`,
	})
}

// policyTypes says which directions a policy isolates for. Left out, the API's
// default applies: ingress always, egress only when the policy has egress rules.
func TestPolicyTypes(t *testing.T) {
	cases := []struct {
		name        string
		policyTypes []PolicyType
		egress      []Rule
		wantIngress bool
		wantEgress  bool
	}{
		{"default without egress rules", nil, nil, true, false},
		{"default with egress rules", nil, []Rule{{}}, true, true},
		{"egress only", []PolicyType{PolicyTypeEgress}, []Rule{{}}, false, true},
		{"ingress only, egress rules ignored", []PolicyType{PolicyTypeIngress}, []Rule{{}}, true, false},
		{"both, no rules at all: deny", []PolicyType{PolicyTypeIngress, PolicyTypeEgress}, nil, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := cluster()
			p.Policies = []NetworkPolicy{{
				Namespace: "default", Name: "p", PodSelector: sel("app", "web"),
				PolicyTypes: tc.policyTypes, Egress: tc.egress,
			}}
			r := build(t, p)

			if got := r.Chain("ingress_default/p") != nil; got != tc.wantIngress {
				t.Errorf("ingress chain: got %v, want %v", got, tc.wantIngress)
			}
			if got := r.Chain("egress_default/p") != nil; got != tc.wantEgress {
				t.Errorf("egress chain: got %v, want %v", got, tc.wantEgress)
			}
			if got := len(elementsOf(t, r, nftables.SetIsolatedIngressV4)) > 0; got != tc.wantIngress {
				t.Errorf("isolated for ingress: got %v, want %v", got, tc.wantIngress)
			}
			if got := len(elementsOf(t, r, nftables.SetIsolatedEgressV4)) > 0; got != tc.wantEgress {
				t.Errorf("isolated for egress: got %v, want %v", got, tc.wantEgress)
			}
		})
	}
}

// Within one peer the two selectors are an AND; separate peers are an OR. A
// namespaceSelector without a podSelector takes every pod of the namespaces, and a
// podSelector without a namespaceSelector stays in the policy's own namespace.
func TestPeerSelectors(t *testing.T) {
	cases := []struct {
		name  string
		peers []Peer
		want  []string
	}{
		{
			name:  "podSelector alone stays in the policy's namespace",
			peers: []Peer{{PodSelector: selp("app", "web")}},
			want:  []string{"192.0.2.10, 198.51.100.10"},
		},
		{
			name:  "namespaceSelector alone takes every pod of the namespaces",
			peers: []Peer{{NamespaceSelector: selp("tier", "trusted")}},
			want:  []string{"198.51.100.11"},
		},
		{
			name:  "both are an AND",
			peers: []Peer{{NamespaceSelector: selp(), PodSelector: selp("app", "web")}},
			want:  []string{"192.0.2.10, 198.51.100.10"},
		},
		{
			name:  "an AND that nothing satisfies",
			peers: []Peer{{NamespaceSelector: selp("tier", "trusted"), PodSelector: selp("app", "web")}},
			want:  nil,
		},
		{
			name:  "separate peers are an OR",
			peers: []Peer{{PodSelector: selp("app", "client")}, {NamespaceSelector: selp("tier", "trusted")}},
			want:  []string{"192.0.2.11", "198.51.100.11"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := cluster()
			p.Policies = []NetworkPolicy{{
				Namespace: "default", Name: "p", PodSelector: sel("app", "web"),
				Ingress: []Rule{{Peers: tc.peers}},
			}}
			r := build(t, p)

			var got []string
			for i := range tc.peers {
				s := r.Set("peer_default/p/in0." + strconv.Itoa(i) + "/v4")
				if s == nil {
					continue
				}
				got = append(got, strings.Join(s.Elements, ", "))
			}
			wantLines(t, "peer sets", got, tc.want)
		})
	}
}

// An ipBlock is matched inline, and its except ranges are cut out with a second,
// negated match on the same address, so that what the entry excludes is still open to
// another rule or another policy (ADR 0003).
func TestIPBlock(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "p", PodSelector: sel("app", "web"),
		PolicyTypes: []PolicyType{PolicyTypeEgress},
		Egress: []Rule{{Peers: []Peer{
			{IPBlock: &IPBlock{CIDR: netip.MustParsePrefix("203.0.113.0/24"),
				Except: []netip.Prefix{netip.MustParsePrefix("203.0.113.128/25")}}},
			{IPBlock: &IPBlock{CIDR: netip.MustParsePrefix("2001:db8:9::/48")}},
		}}},
	}}
	r := build(t, p)

	wantLines(t, "egress_default/p", rulesOf(t, r, "egress_default/p"), []string{
		`ip daddr 203.0.113.0/24 ip daddr != { 203.0.113.128/25 } counter accept comment "default/p"`,
		`ip6 daddr 2001:db8:9::/48 counter accept comment "default/p"`,
	})
}

// A port entry with a number, a range or nothing at all is a match on the transport
// header alone. A named port is resolved from the container ports of the pods that
// receive the traffic, and the rule names those pods, since another pod may give the
// same name a different number or not declare it at all.
func TestPorts(t *testing.T) {
	p := cluster()
	p.Pods[0].Ports = []NamedPort{{Name: "http", Port: 8080, Protocol: ProtocolTCP}}
	p.Pods[1].Ports = []NamedPort{{Name: "http", Port: 9090, Protocol: ProtocolTCP}}
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "p", PodSelector: sel(),
		Ingress: []Rule{{Ports: []Port{
			{Protocol: ProtocolTCP, Number: 80},
			{Protocol: ProtocolUDP, Number: 30000, EndPort: 30100},
			{Protocol: ProtocolSCTP},
			{Name: "http"},
		}}},
	}}
	r := build(t, p)

	wantLines(t, "ingress_default/p", rulesOf(t, r, "ingress_default/p"), []string{
		`tcp dport 80 counter accept comment "default/p"`,
		`udp dport 30000-30100 counter accept comment "default/p"`,
		`meta l4proto sctp counter accept comment "default/p"`,
		`ip daddr @port_default/p/in0.0.3/8080/v4 tcp dport 8080 counter accept comment "default/p"`,
		`ip6 daddr @port_default/p/in0.0.3/8080/v6 tcp dport 8080 counter accept comment "default/p"`,
		`ip daddr @port_default/p/in0.0.3/9090/v4 tcp dport 9090 counter accept comment "default/p"`,
		`ip6 daddr @port_default/p/in0.0.3/9090/v6 tcp dport 9090 counter accept comment "default/p"`,
	})
	wantLines(t, "the pods that call 8080 http", elementsOf(t, r, "port_default/p/in0.0.3/8080/v4"), []string{"192.0.2.10"})
	wantLines(t, "the pods that call 9090 http", elementsOf(t, r, "port_default/p/in0.0.3/9090/v4"), []string{"192.0.2.11"})
}

// An endPort below the port it starts from is not a range.
func TestAnInvertedPortRangeIsAnError(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "p", PodSelector: sel("app", "web"),
		Ingress: []Rule{{Ports: []Port{{Number: 200, EndPort: 100}}}},
	}}
	r, err := nftables.Render(nodeParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := Add(r, p); err == nil {
		t.Error("Add accepted endPort 100 under port 200")
	}
}

// An empty "from" is every peer and empty "ports" is every port, so the rule is a
// bare accept. An egress rule's peers are matched on the destination.
func TestEmptyPeersAndEmptyPorts(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "p", PodSelector: sel("app", "web"),
		PolicyTypes: []PolicyType{PolicyTypeIngress, PolicyTypeEgress},
		Ingress:     []Rule{{}},
		Egress:      []Rule{{Peers: []Peer{{PodSelector: selp("app", "client")}}, Ports: []Port{{Number: 53, Protocol: ProtocolUDP}}}},
	}}
	r := build(t, p)

	wantLines(t, "ingress_default/p", rulesOf(t, r, "ingress_default/p"), []string{
		`counter accept comment "default/p"`,
	})
	wantLines(t, "egress_default/p", rulesOf(t, r, "egress_default/p"), []string{
		`ip daddr @peer_default/p/eg0.0/v4 udp dport 53 counter accept comment "default/p"`,
		`ip6 daddr @peer_default/p/eg0.0/v6 udp dport 53 counter accept comment "default/p"`,
	})
	wantLines(t, "egress_dispatch", rulesOf(t, r, nftables.ChainEgressDispatch), []string{
		`ip saddr @pods_default/p/v4 counter jump egress_default/p comment "default/p"`,
		`ip6 saddr @pods_default/p/v6 counter jump egress_default/p comment "default/p"`,
		`counter drop comment "isolated pod, no policy accepted"`,
	})
}

// A policy that selects no pod of this node adds nothing: the pod it governs is
// enforced by the node that pod runs on (ADR 0003).
func TestOnlyThisNodesPodsAreEnforcedHere(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "other", Name: "p", PodSelector: sel("app", "probe"),
		Ingress: []Rule{{}},
	}}
	r := build(t, p)

	if r.Chain("ingress_other/p") != nil {
		t.Error("a policy that selects nothing on this node got a chain")
	}
	wantLines(t, "isolated_ingress_v4", elementsOf(t, r, nftables.SetIsolatedIngressV4), nil)
}

// A pod on the host network has the node's addresses, and a pod without an address is
// not running yet. Neither is selected and neither is a peer.
func TestHostNetworkAndAddresslessPodsAreIgnored(t *testing.T) {
	p := cluster()
	p.Pods = append(p.Pods,
		Pod{Namespace: "default", Name: "hostnet", NodeName: "node-a", Labels: map[string]string{"app": "web"},
			IPs: ips("203.0.113.1"), HostNetwork: true},
		Pod{Namespace: "default", Name: "pending", NodeName: "node-a", Labels: map[string]string{"app": "web"}},
	)
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "p", PodSelector: sel("app", "web"),
		Ingress: []Rule{{Peers: []Peer{{PodSelector: selp("app", "web")}}}},
	}}
	r := build(t, p)

	wantLines(t, "isolated_ingress_v4", elementsOf(t, r, nftables.SetIsolatedIngressV4), []string{"192.0.2.10"})
	wantLines(t, "the peer set", elementsOf(t, r, "peer_default/p/in0.0/v4"), []string{"192.0.2.10", "198.51.100.10"})
}

// A name nft could not read back is an error, not text that would parse as something
// else (ADR 0003).
func TestANameNFTCannotReadIsAnError(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "web in", PodSelector: sel("app", "web"),
		Ingress: []Rule{{}},
	}}
	r, err := nftables.Render(nodeParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := Add(r, p); err == nil {
		t.Error("Add accepted a name with a space in it")
	}
}

// A namespace and a name together can be longer than an nftables identifier, which is
// reachable without anybody doing anything odd. The name is folded rather than
// refused, so one long-named policy does not stop the node's table being updated. The
// whole of the folded table is in the golden file.
func TestALongNameIsFoldedRatherThanRefused(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: longName, PodSelector: sel("app", "web"),
		Ingress: []Rule{{Peers: []Peer{{PodSelector: selp("app", "client")}}}},
	}}
	r := build(t, p)

	chain := r.Chains[len(r.Chains)-1]
	if len(chain.Name) > 255 || len(chain.Rules) != 2 {
		t.Fatalf("the long-named policy did not render: %q with %d rules", chain.Name, len(chain.Rules))
	}
	for _, set := range r.Sets {
		if len(set.Name) > 255 {
			t.Errorf("set %q is longer than nft allows", set.Name)
		}
	}
	wantLines(t, "isolated_ingress_v4", elementsOf(t, r, nftables.SetIsolatedIngressV4), []string{"192.0.2.10"})
}

// A pod that has run to its end keeps its addresses in the API until it is deleted,
// and by then one of them may belong to a pod that is running. Leaving it in a set
// would open that new pod to whatever the dead pod's labels asked for.
func TestPodsThatHaveFinishedAreIgnored(t *testing.T) {
	p := cluster()
	// The finished pod carries the label the policy selects and the label its peer
	// entry names, and holds the address its live namesake would be given next.
	p.Pods = append(p.Pods, Pod{
		Namespace: "default", Name: "done", NodeName: "node-a",
		Labels: map[string]string{"app": "web"}, IPs: ips("192.0.2.12", "2001:db8:1::12"),
		Terminated: true,
	})
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "p", PodSelector: sel("app", "web"),
		Ingress: []Rule{{Peers: []Peer{{PodSelector: selp("app", "web")}}}},
	}}
	r := build(t, p)

	wantLines(t, "isolated_ingress_v4", elementsOf(t, r, nftables.SetIsolatedIngressV4), []string{"192.0.2.10"})
	wantLines(t, "the peer set", elementsOf(t, r, "peer_default/p/in0.0/v4"), []string{"192.0.2.10", "198.51.100.10"})
}

// A selector the API would have rejected is a render error rather than a policy that
// quietly governs nothing.
func TestABadSelectorIsAnError(t *testing.T) {
	p := cluster()
	p.Policies = []NetworkPolicy{{
		Namespace: "default", Name: "p",
		PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: "Nonsense", Values: []string{"web"}},
		}},
		Ingress: []Rule{{}},
	}}
	r, err := nftables.Render(nodeParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := Add(r, p); err == nil {
		t.Error("Add accepted an operator no selector has")
	}
}

// The whole table, skeleton and policies together, is fixed by a golden file so that
// a change to it is a deliberate, reviewed diff. The netns tests apply the same case.
func TestAddMatchesGolden(t *testing.T) {
	r := build(t, goldenParams())
	got := r.String()
	golden := filepath.Join("testdata", "policies.nft")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("rendered table differs from %s.\n"+
			"Read the difference before accepting it; run `go test ./internal/netpol -update` to take it.\n"+
			"--- got ---\n%s", golden, got)
	}
}

// longName is as long as a Kubernetes name may be, which is long enough that the
// nftables names rendered from it do not fit and have to be folded.
var longName = strings.Repeat("a-very-long-policy-name-", 10)

// goldenParams is one cluster with a policy for each shape the semantics have: a
// deny-all, an ingress policy with two peers, ports and an ipBlock with more than one
// except, an egress policy with a named port, and one whose name has to be folded.
func goldenParams() Params {
	p := cluster()
	p.Pods[0].Ports = []NamedPort{{Name: "http", Port: 8080, Protocol: ProtocolTCP}}
	p.Pods[2].Ports = []NamedPort{{Name: "http", Port: 8080, Protocol: ProtocolTCP}}
	p.Policies = []NetworkPolicy{
		{
			Namespace: "default", Name: "deny-all", PodSelector: sel(),
			PolicyTypes: []PolicyType{PolicyTypeIngress, PolicyTypeEgress},
		},
		{
			Namespace: "default", Name: "web-in", PodSelector: sel("app", "web"),
			Ingress: []Rule{
				{
					Peers: []Peer{{PodSelector: selp("app", "client")}, {NamespaceSelector: selp("tier", "trusted")}},
					Ports: []Port{{Protocol: ProtocolTCP, Number: 80}, {Protocol: ProtocolTCP, Number: 8000, EndPort: 8100}},
				},
				{Peers: []Peer{{IPBlock: &IPBlock{
					CIDR: netip.MustParsePrefix("203.0.113.0/24"),
					Except: []netip.Prefix{
						netip.MustParsePrefix("203.0.113.16/28"),
						netip.MustParsePrefix("203.0.113.128/25"),
					},
				}}}},
			},
		},
		{
			Namespace: "default", Name: "client-out", PodSelector: sel("app", "client"),
			PolicyTypes: []PolicyType{PolicyTypeEgress},
			Egress: []Rule{
				{Peers: []Peer{{PodSelector: selp("app", "web")}}, Ports: []Port{{Name: "http"}}},
				{Ports: []Port{{Protocol: ProtocolUDP, Number: 53}}},
			},
		},
		{
			Namespace: "default", Name: longName, PodSelector: sel("app", "web"),
			Ingress: []Rule{{Peers: []Peer{{PodSelector: selp("app", "client")}}}},
		},
	}
	return p
}
