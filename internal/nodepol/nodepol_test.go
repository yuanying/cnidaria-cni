package nodepol

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yuanying/cnidaria-cni/internal/apis/v1alpha1"
	"github.com/yuanying/cnidaria-cni/internal/nftables"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the renderer produces")

// node is the node every case renders for: a control-plane node named node-a.
var node = Node{
	Name: "node-a",
	Labels: map[string]string{
		"kubernetes.io/hostname":                "node-a",
		"node-role.kubernetes.io/control-plane": "",
	},
}

func prefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s))
	for _, p := range s {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// base is the table before any policy: what nftables.Render produces for a dual-stack
// node. The golden files hold the whole table, so a jump landing in the wrong place is
// a visible diff.
func base(t testing.TB) *nftables.Ruleset {
	t.Helper()
	rs, err := nftables.Render(nftables.Params{
		PodCIDRs:        prefixes("192.0.2.0/24", "2001:db8:1::/64"),
		ClusterPodCIDRs: prefixes("192.0.2.0/24", "2001:db8:1::/64", "198.51.100.0/24", "2001:db8:2::/64"),
		SafePorts:       nftables.DefaultSafePorts,
	})
	if err != nil {
		t.Fatalf("nftables.Render: %v", err)
	}
	return rs
}

func policy(name string, mode v1alpha1.Mode, spec v1alpha1.NodePolicySpec) v1alpha1.NodePolicy {
	spec.Mode = mode
	return v1alpha1.NodePolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

func ipBlock(cidr string, except ...string) v1alpha1.NodePolicyPeer {
	return v1alpha1.NodePolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr, Except: except}}
}

func port(proto corev1.Protocol, p int32) v1alpha1.NodePolicyPort {
	return v1alpha1.NodePolicyPort{Protocol: proto, Port: ptr(p)}
}

var goldenCases = []struct {
	name     string
	policies []v1alpha1.NodePolicy
}{
	{
		// A policy that selects another node leaves this node's table alone: both
		// directions stay open, dispatch chains and all.
		name: "not-selected",
		policies: []v1alpha1.NodePolicy{
			policy("storage", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
				NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "storage"}},
				Ingress: []v1alpha1.NodePolicyIngressRule{
					{Ports: []v1alpha1.NodePolicyPort{port(corev1.ProtocolTCP, 3260)}},
				},
			}),
		},
	},
	{
		// The default: everything is rendered, nothing is dropped.
		name: "permissive-ingress",
		policies: []v1alpha1.NodePolicy{
			policy("metrics", v1alpha1.ModePermissive, v1alpha1.NodePolicySpec{
				Ingress: []v1alpha1.NodePolicyIngressRule{{
					From:  []v1alpha1.NodePolicyPeer{ipBlock("192.0.2.0/24")},
					Ports: []v1alpha1.NodePolicyPort{port(corev1.ProtocolTCP, 9100)},
				}},
			}),
		},
	},
	{
		// Two peers and two ports in one rule are four rules: every combination the
		// entry allows, one nft rule each.
		name: "enforce-ingress",
		policies: []v1alpha1.NodePolicy{
			policy("control-plane", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
				NodeSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
				},
				Ingress: []v1alpha1.NodePolicyIngressRule{{
					From: []v1alpha1.NodePolicyPeer{ipBlock("198.51.100.0/24"), ipBlock("2001:db8:2::/64")},
					Ports: []v1alpha1.NodePolicyPort{
						port(corev1.ProtocolTCP, 9100),
						port(corev1.ProtocolUDP, 8472),
					},
				}},
			}),
		},
	},
	{
		// One permissive policy keeps the node observing, however many enforcing
		// ones share the direction with it (ADR 0004). The policies are rendered in
		// name order whatever order they arrived in.
		name: "mixed-modes",
		policies: []v1alpha1.NodePolicy{
			policy("ssh-only", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
				Ingress: []v1alpha1.NodePolicyIngressRule{{
					Ports: []v1alpha1.NodePolicyPort{port(corev1.ProtocolTCP, 22)},
				}},
			}),
			policy("metrics", v1alpha1.ModePermissive, v1alpha1.NodePolicySpec{
				Ingress: []v1alpha1.NodePolicyIngressRule{{
					Ports: []v1alpha1.NodePolicyPort{port(corev1.ProtocolTCP, 9100)},
				}},
			}),
		},
	},
	{
		// The exceptions of an ipBlock become a negated anonymous set next to the
		// prefix, so the rule is still one rule.
		name: "ipblock-except",
		policies: []v1alpha1.NodePolicy{
			policy("lan", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
				Ingress: []v1alpha1.NodePolicyIngressRule{{
					From: []v1alpha1.NodePolicyPeer{
						ipBlock("192.0.2.0/24", "192.0.2.128/25", "192.0.2.64/26"),
						ipBlock("2001:db8:2::/64", "2001:db8:2:0:8000::/65"),
					},
					Ports: []v1alpha1.NodePolicyPort{port(corev1.ProtocolTCP, 10250)},
				}},
			}),
		},
	},
	{
		// A range, and an entry with a protocol but no port, which covers the whole
		// protocol.
		name: "port-range",
		policies: []v1alpha1.NodePolicy{
			policy("services", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
				Ingress: []v1alpha1.NodePolicyIngressRule{{
					Ports: []v1alpha1.NodePolicyPort{
						{Protocol: corev1.ProtocolTCP, Port: ptr[int32](30000), EndPort: ptr[int32](30100)},
						{Protocol: corev1.ProtocolSCTP},
					},
				}},
			}),
		},
	},
	{
		// policyTypes chooses the direction: this one closes egress only, so input
		// keeps its safe rules and nothing else.
		name: "egress-only",
		policies: []v1alpha1.NodePolicy{
			policy("upstream", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
				PolicyTypes: []v1alpha1.PolicyType{v1alpha1.PolicyTypeEgress},
				Egress: []v1alpha1.NodePolicyEgressRule{
					{
						To:    []v1alpha1.NodePolicyPeer{ipBlock("203.0.113.0/24")},
						Ports: []v1alpha1.NodePolicyPort{port(corev1.ProtocolTCP, 443)},
					},
					{
						To: []v1alpha1.NodePolicyPeer{ipBlock("2001:db8:3::/64")},
					},
				},
				// Ingress rules are present but not listed in policyTypes, so they
				// are not rendered.
				Ingress: []v1alpha1.NodePolicyIngressRule{{
					Ports: []v1alpha1.NodePolicyPort{port(corev1.ProtocolTCP, 22)},
				}},
			}),
		},
	},
	{
		// A policy that lists no rules at all closes both directions down to the
		// safe rules. Egress is closed because egress rules are absent but listed.
		name: "deny-all",
		policies: []v1alpha1.NodePolicy{
			policy("lockdown", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
				PolicyTypes: []v1alpha1.PolicyType{v1alpha1.PolicyTypeIngress, v1alpha1.PolicyTypeEgress},
			}),
		},
	},
}

// The table a node ends up with is the whole of what a NodePolicy does, so its shape
// is fixed by golden files and a change to it is a reviewed diff.
func TestAddMatchesGolden(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			rs := base(t)
			if _, err := Add(rs, node, tc.policies); err != nil {
				t.Fatalf("Add: %v", err)
			}
			got := rs.String()
			golden := filepath.Join("testdata", tc.name+".nft")
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
					"Read the difference before accepting it; run `go test ./internal/nodepol -update` to take it.\n"+
					"--- got ---\n%s", golden, got)
			}
		})
	}
}

// What a node reports back per policy (ADR 0004): the mode that policy took effect
// in, which is Enforce only where nothing permissive shares the direction.
func TestAddReportsTheModeEachPolicyTookEffectIn(t *testing.T) {
	byName := map[string][]v1alpha1.NodePolicy{}
	for _, tc := range goldenCases {
		byName[tc.name] = tc.policies
	}
	cases := map[string]map[string]v1alpha1.Mode{
		"not-selected":       {},
		"permissive-ingress": {"metrics": v1alpha1.ModePermissive},
		"enforce-ingress":    {"control-plane": v1alpha1.ModeEnforce},
		// The enforcing policy reads Permissive: nothing is being dropped.
		"mixed-modes": {"ssh-only": v1alpha1.ModePermissive, "metrics": v1alpha1.ModePermissive},
		"egress-only": {"upstream": v1alpha1.ModeEnforce},
		"deny-all":    {"lockdown": v1alpha1.ModeEnforce},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Add(base(t), node, byName[name])
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("modes = %v, want %v", got, want)
			}
			for policy, mode := range want {
				if got[policy] != mode {
					t.Errorf("mode of %s = %q, want %q", policy, got[policy], mode)
				}
			}
		})
	}
}

// A policy that is permissive in one direction and enforcing in the other is not
// enforcing as far as the operator reading its status is concerned.
func TestAPolicyEnforcingOnlyOneDirectionReadsAsPermissive(t *testing.T) {
	both := policy("both", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
		PolicyTypes: []v1alpha1.PolicyType{v1alpha1.PolicyTypeIngress, v1alpha1.PolicyTypeEgress},
	})
	watching := policy("watching", v1alpha1.ModePermissive, v1alpha1.NodePolicySpec{
		PolicyTypes: []v1alpha1.PolicyType{v1alpha1.PolicyTypeEgress},
	})
	got, err := Add(base(t), node, []v1alpha1.NodePolicy{both, watching})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got["both"] != v1alpha1.ModePermissive {
		t.Errorf("mode of both = %q, want %q", got["both"], v1alpha1.ModePermissive)
	}
}

// Egress defaults to closed only when the policy carries egress rules, as
// NetworkPolicy's policyTypes do (ADR 0004).
func TestEgressIsClosedOnlyWhenItHasRulesOrIsListed(t *testing.T) {
	ingressOnly := policy("ingress-only", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
		Ingress: []v1alpha1.NodePolicyIngressRule{{}},
	})
	rs := base(t)
	if _, err := Add(rs, node, []v1alpha1.NodePolicy{ingressOnly}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if text := rs.String(); strings.Contains(text, ChainOutputDispatch) {
		t.Errorf("a policy with no egress rules closed egress:\n%s", text)
	}

	withEgress := policy("with-egress", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
		Egress: []v1alpha1.NodePolicyEgressRule{{}},
	})
	rs = base(t)
	if _, err := Add(rs, node, []v1alpha1.NodePolicy{withEgress}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if text := rs.String(); !strings.Contains(text, ChainOutputDispatch) {
		t.Errorf("a policy with egress rules left egress open:\n%s", text)
	}
}

// Selection is a label selector like any other, matchExpressions included.
func TestAddSelectsWithMatchExpressions(t *testing.T) {
	workers := policy("workers", v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{
		NodeSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: metav1.LabelSelectorOpDoesNotExist,
		}}},
	})
	got, err := Add(base(t), node, []v1alpha1.NodePolicy{workers})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a control-plane node was selected by a policy that excludes it: %v", got)
	}
}

// Anything that would render into text nft refuses, or into a rule that says
// something other than what the object says, is an error the node can report in
// status rather than a broken table.
func TestAddRejectsBadInput(t *testing.T) {
	cases := map[string]v1alpha1.NodePolicySpec{
		"peer without an ipBlock": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{From: []v1alpha1.NodePolicyPeer{{}}}},
		},
		"cidr that is not a prefix": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{From: []v1alpha1.NodePolicyPeer{ipBlock("192.0.2.1")}}},
		},
		"cidr with bits below its prefix length": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{From: []v1alpha1.NodePolicyPeer{ipBlock("192.0.2.1/24")}}},
		},
		"except with bits below its prefix length": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				From: []v1alpha1.NodePolicyPeer{ipBlock("192.0.2.0/24", "192.0.2.129/25")},
			}},
		},
		"except of another family": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				From: []v1alpha1.NodePolicyPeer{ipBlock("192.0.2.0/24", "2001:db8::/64")},
			}},
		},
		"except that is not a prefix": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				From: []v1alpha1.NodePolicyPeer{ipBlock("192.0.2.0/24", "nonsense")},
			}},
		},
		"endPort without a port": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				Ports: []v1alpha1.NodePolicyPort{{Protocol: corev1.ProtocolTCP, EndPort: ptr[int32](80)}},
			}},
		},
		"endPort below the port": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				Ports: []v1alpha1.NodePolicyPort{{
					Protocol: corev1.ProtocolTCP, Port: ptr[int32](90), EndPort: ptr[int32](80),
				}},
			}},
		},
		"port out of range": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				Ports: []v1alpha1.NodePolicyPort{{Protocol: corev1.ProtocolTCP, Port: ptr[int32](70000)}},
			}},
		},
		"protocol nft has no keyword for": {
			Ingress: []v1alpha1.NodePolicyIngressRule{{
				Ports: []v1alpha1.NodePolicyPort{{Protocol: corev1.Protocol("HTTP")}},
			}},
		},
		"selector that cannot be read": {
			NodeSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "role", Operator: metav1.LabelSelectorOperator("sortof"),
			}}},
		},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			rs := base(t)
			before := rs.String()
			if _, err := Add(rs, node, []v1alpha1.NodePolicy{policy("bad", v1alpha1.ModeEnforce, spec)}); err == nil {
				t.Fatalf("Add accepted it:\n%s", rs.String())
			} else if !strings.Contains(err.Error(), "bad") {
				t.Errorf("error %q does not name the policy", err)
			}
			if rs.String() != before {
				t.Errorf("a rejected policy left something behind:\n%s", rs.String())
			}
		})
	}
}

// A name longer than nft takes is not a reason to refuse a policy. The nftables
// package folds the chain name and cuts the comment where the text is written, so a
// node whose policy carries a long name still gets its table.
func TestALongNameIsCarriedRatherThanRefused(t *testing.T) {
	long := strings.Repeat("a", 250)
	rs := base(t)
	if _, err := Add(rs, node, []v1alpha1.NodePolicy{
		policy(long, v1alpha1.ModeEnforce, v1alpha1.NodePolicySpec{}),
	}); err != nil {
		t.Fatalf("Add refused a policy over the length of its name: %v", err)
	}
	for _, chain := range rs.Chains {
		if len(chain.Name) > 255 {
			t.Errorf("chain name %q is %d bytes, more than nft takes", chain.Name, len(chain.Name))
		}
	}
	if text := rs.String(); strings.Contains(text, comment(long)) {
		t.Errorf("the comment was written out in full rather than cut to what nft takes:\n%s", text)
	}
}
