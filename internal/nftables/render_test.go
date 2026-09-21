package nftables

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the renderer produces")

func prefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s))
	for _, p := range s {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

// goldenCases are the inputs whose rendering is fixed by a file under testdata. The
// netns tests apply the same cases, so what nft is shown to accept is what the golden
// files hold.
var goldenCases = []struct {
	name   string
	params Params
}{
	{
		name: "dual-stack",
		params: Params{
			PodCIDRs:        prefixes("192.0.2.0/24", "2001:db8:1::/64"),
			ClusterPodCIDRs: prefixes("192.0.2.0/24", "2001:db8:1::/64", "198.51.100.0/24", "2001:db8:2::/64"),
			SafePorts:       DefaultSafePorts,
		},
	},
	{
		name: "ipv4-only",
		params: Params{
			PodCIDRs:        prefixes("192.0.2.0/24"),
			ClusterPodCIDRs: prefixes("192.0.2.0/24"),
			SafePorts:       DefaultSafePorts,
		},
	},
	{
		// The cluster's CIDRs arrive in the order the nodes were listed in, with
		// this node in the middle. The sets come out sorted whatever the order.
		name: "three-nodes",
		params: Params{
			PodCIDRs: prefixes("198.51.100.0/24", "2001:db8:2::/64"),
			ClusterPodCIDRs: prefixes(
				"203.0.113.0/24", "2001:db8:3::/64",
				"198.51.100.0/24", "2001:db8:2::/64",
				"192.0.2.0/24", "2001:db8:1::/64",
			),
			SafePorts: DefaultSafePorts,
		},
	},
}

// The table is the whole of what cnidaria puts on a node (ADR 0003). Its shape is fixed
// by golden files so that a change to it is a deliberate, reviewed diff.
func TestRenderMatchesGolden(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			ruleset, err := Render(tc.params)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			got := ruleset.String()
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
					"Read the difference before accepting it; run `go test ./internal/nftables -update` to take it.\n"+
					"--- got ---\n%s", golden, got)
			}
		})
	}
}

// The ports the safe rules open are arguments to the daemon (ADR 0004), so a changed
// port has to reach the text.
func TestRenderUsesTheGivenSafePorts(t *testing.T) {
	p := goldenCases[0].params
	p.SafePorts.SSH = 2222
	p.SafePorts.NodePort = PortRange{From: 40000, To: 40000}
	ruleset, err := Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := ruleset.String()
	for _, want := range []string{
		`tcp dport 2222 counter accept comment "safe: ssh"`,
		`tcp dport 40000 counter accept comment "safe: nodeport"`,
		`udp dport 40000 counter accept comment "safe: nodeport"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered table lacks %q", want)
		}
	}
	if strings.Contains(got, "tcp dport 22 ") {
		t.Errorf("rendered table still opens port 22")
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	good := goldenCases[0].params
	cases := map[string]func(p *Params){
		"no pod CIDR":          func(p *Params) { p.PodCIDRs = nil },
		"two IPv4 pod CIDRs":   func(p *Params) { p.PodCIDRs = prefixes("192.0.2.0/24", "198.51.100.0/24") },
		"invalid pod CIDR":     func(p *Params) { p.PodCIDRs = []netip.Prefix{{}} },
		"invalid cluster CIDR": func(p *Params) { p.ClusterPodCIDRs = []netip.Prefix{{}} },
		"zero ssh port":        func(p *Params) { p.SafePorts.SSH = 0 },
		"zero kubelet port":    func(p *Params) { p.SafePorts.Kubelet = 0 },
		"zero apiserver port":  func(p *Params) { p.SafePorts.APIServer = 0 },
		"zero dns port":        func(p *Params) { p.SafePorts.DNS = 0 },
		"empty etcd range":     func(p *Params) { p.SafePorts.Etcd = PortRange{} },
		"inverted nodeport":    func(p *Params) { p.SafePorts.NodePort = PortRange{From: 32767, To: 30000} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := good
			mutate(&p)
			if _, err := Render(p); err == nil {
				t.Errorf("Render accepted %+v", p)
			}
		})
	}
}

// Names from the API become nftables identifiers behind a prefix per kind. The join
// character is one no Kubernetes name may contain, so two objects never collide.
func TestIdentifier(t *testing.T) {
	cases := []struct {
		prefix string
		parts  []string
		want   string
	}{
		{"policy_", []string{"default", "web"}, "policy_default/web"},
		{"pods_", []string{"kube-system", "core.dns-1"}, "pods_kube-system/core.dns-1"},
		{"nodenetworkpolicy_", []string{"control-plane"}, "nodenetworkpolicy_control-plane"},
	}
	for _, tc := range cases {
		got, err := Identifier(tc.prefix, tc.parts...)
		if err != nil {
			t.Errorf("Identifier(%q, %q): %v", tc.prefix, tc.parts, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Identifier(%q, %q) = %q, want %q", tc.prefix, tc.parts, got, tc.want)
		}
	}
}

func TestIdentifierRejectsWhatNFTCannotRead(t *testing.T) {
	cases := map[string][]string{
		"no parts":   nil,
		"empty part": {"default", ""},
		"space":      {"default", "web app"},
		"slash":      {"default", "a/b"},
		"non-ascii":  {"default", "ウェブ"},
		"quote":      {"default", `a"b`},
	}
	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := Identifier("policy_", parts...); err == nil {
				t.Errorf("Identifier accepted %q as %q", parts, got)
			}
		})
	}
}

// A namespace and a name can be 63 and 253 bytes, so a name longer than nft accepts is
// reachable without anybody doing anything odd. It is folded onto a hash of itself
// rather than refused, since refusing it would stop the node's whole table being
// updated (ADR 0003).
func TestALongIdentifierIsFoldedOntoAHashOfItself(t *testing.T) {
	long := []string{strings.Repeat("a", 63), strings.Repeat("b", 253), "v4"}
	got, err := Identifier("port_", long...)
	if err != nil {
		t.Fatalf("Identifier: %v", err)
	}
	if len(got) != identifierMaxLen {
		t.Errorf("the folded name is %d bytes, want %d: %q", len(got), identifierMaxLen, got)
	}
	// What nft's lexer reads back as one identifier.
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./-]*$`).MatchString(got) {
		t.Errorf("the folded name is not one nft can read: %q", got)
	}
	if !strings.HasPrefix(got, "port_"+strings.Repeat("a", 63)+"/") {
		t.Errorf("the folded name no longer says where it came from: %q", got)
	}
	// The same name folds the same way on every node, and two names that share the
	// head they keep are still told apart.
	again, err := Identifier("port_", long...)
	if err != nil || again != got {
		t.Errorf("Identifier is not a function of its input: %q then %q (%v)", got, again, err)
	}
	other := append(append([]string{}, long[:2]...), "v6")
	sibling, err := Identifier("port_", other...)
	if err != nil {
		t.Fatal(err)
	}
	if sibling == got {
		t.Errorf("two names folded onto one: %q", got)
	}
}

// nft refuses a comment longer than 128 bytes, and with it the whole file. A
// namespace and a name together go well past that, so what is handed to nft is cut.
func TestALongCommentIsClipped(t *testing.T) {
	rule := Rule{Verdict: "accept", Comment: "default/" + strings.Repeat("n", 253)}
	got := rule.String()
	comment := got[strings.Index(got, `comment "`)+len(`comment "`) : len(got)-1]
	if len(comment) != commentMaxLen {
		t.Errorf("the comment nft is handed is %d bytes, want %d: %q", len(comment), commentMaxLen, comment)
	}
	if !strings.HasPrefix(comment, "default/nnn") || !strings.HasSuffix(comment, "...") {
		t.Errorf("the clipped comment neither says where it came from nor that it was cut: %q", comment)
	}
	short := Rule{Verdict: "accept", Comment: "default/web"}
	if !strings.HasSuffix(short.String(), `comment "default/web"`) {
		t.Errorf("a comment that fits was changed: %q", short.String())
	}
}
