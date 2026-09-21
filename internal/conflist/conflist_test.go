package conflist

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The conflist is the whole of what cnidaria hands to the container runtime (ADR 0001).
// Its shape is fixed by golden files so that a change to it is a deliberate, reviewed
// diff and not a side effect.
func TestRenderMatchesGolden(t *testing.T) {
	cases := []struct {
		name   string
		params Params
	}{
		{
			name: "dual-stack",
			params: Params{
				Name:   "cnidaria",
				Bridge: "cni0",
				MTU:    1500,
				PodCIDRs: []netip.Prefix{
					netip.MustParsePrefix("192.0.2.0/24"),
					netip.MustParsePrefix("2001:db8:1::/64"),
				},
			},
		},
		{
			name: "ipv4-only",
			params: Params{
				Name:     "cnidaria",
				Bridge:   "cni0",
				MTU:      1500,
				PodCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
			},
		},
		{
			// host-local keeps its leases under the network name, so a node taking
			// over from another CNI names its network after that one's to share the
			// allocations with the pods that already exist (ADR 0009).
			name: "network-name",
			params: Params{
				Name:   "cbr0",
				Bridge: "cni0",
				MTU:    1500,
				PodCIDRs: []netip.Prefix{
					netip.MustParsePrefix("192.0.2.0/24"),
					netip.MustParsePrefix("2001:db8:1::/64"),
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(tc.params)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			golden := filepath.Join("testdata", tc.name+".conflist")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("rendered conflist differs from %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
			}
		})
	}
}

func TestRenderRejectsMissingInput(t *testing.T) {
	cases := map[string]Params{
		"no pod CIDR": {Name: "cnidaria", Bridge: "cni0", MTU: 1500},
		"no name":     {Bridge: "cni0", MTU: 1500, PodCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}},
		"no bridge":   {Name: "cnidaria", MTU: 1500, PodCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}},
		"two IPv4 ranges": {Name: "cnidaria", Bridge: "cni0", MTU: 1500, PodCIDRs: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"),
		}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Render(p); err == nil {
				t.Errorf("Render(%+v) returned no error", p)
			}
		})
	}
}

// The rendered document ends with a newline so that it diffs cleanly on the node.
func TestRenderEndsWithNewline(t *testing.T) {
	got, err := Render(Params{
		Name: "cnidaria", Bridge: "cni0", MTU: 1500,
		PodCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("output does not end with a newline")
	}
}
