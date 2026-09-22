package routes

import (
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr  { return netip.MustParseAddr(s) }

// Compute is the whole of ADR 0006 as a function: one route per peer node and
// family, next hop that node's InternalIP of the same family, nothing when either
// side of a family is missing, and nothing for this node itself.
func TestCompute(t *testing.T) {
	self := Node{
		Name:        "node-a",
		PodCIDRs:    []netip.Prefix{pfx("192.0.2.0/24"), pfx("2001:db8:a::/64")},
		InternalIPs: []netip.Addr{addr("203.0.113.1"), addr("2001:db8::1")},
	}
	cases := []struct {
		name        string
		nodes       []Node
		wantRoutes  []Route
		wantMissing []Missing
	}{
		{
			name:  "no peers gives no routes",
			nodes: []Node{self},
		},
		{
			name: "both families on the peer give one route per family",
			nodes: []Node{self, {
				Name:        "node-b",
				PodCIDRs:    []netip.Prefix{pfx("198.51.100.0/24"), pfx("2001:db8:b::/64")},
				InternalIPs: []netip.Addr{addr("203.0.113.2"), addr("2001:db8::2")},
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("198.51.100.0/24"), Via: addr("203.0.113.2")},
				{Node: "node-b", Dst: pfx("2001:db8:b::/64"), Via: addr("2001:db8::2")},
			},
		},
		{
			name: "a peer with an IPv6 pod CIDR but no IPv6 InternalIP gets only the IPv4 route and a warning",
			nodes: []Node{self, {
				Name:        "node-b",
				PodCIDRs:    []netip.Prefix{pfx("198.51.100.0/24"), pfx("2001:db8:b::/64")},
				InternalIPs: []netip.Addr{addr("203.0.113.2")},
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("198.51.100.0/24"), Via: addr("203.0.113.2")},
			},
			wantMissing: []Missing{{Node: "node-b", Family: "IPv6", PodCIDR: pfx("2001:db8:b::/64")}},
		},
		{
			// Kubelet reports one InternalIP unless told otherwise, so on many
			// clusters the peer's IPv6 address is only in its annotation.
			name: "a peer with no IPv6 InternalIP is reached over IPv6 through the address it annotated",
			nodes: []Node{self, {
				Name:          "node-b",
				PodCIDRs:      []netip.Prefix{pfx("198.51.100.0/24"), pfx("2001:db8:b::/64")},
				InternalIPs:   []netip.Addr{addr("203.0.113.2")},
				AnnotatedIPv6: addr("2001:db8::2"),
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("198.51.100.0/24"), Via: addr("203.0.113.2")},
				{Node: "node-b", Dst: pfx("2001:db8:b::/64"), Via: addr("2001:db8::2")},
			},
		},
		{
			name: "an IPv6 InternalIP wins over the annotated address",
			nodes: []Node{self, {
				Name:          "node-b",
				PodCIDRs:      []netip.Prefix{pfx("2001:db8:b::/64")},
				InternalIPs:   []netip.Addr{addr("203.0.113.2"), addr("2001:db8::2")},
				AnnotatedIPv6: addr("2001:db8::22"),
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("2001:db8:b::/64"), Via: addr("2001:db8::2")},
			},
		},
		{
			name: "a peer with an IPv4 InternalIP but no IPv4 pod CIDR is not a warning",
			nodes: []Node{self, {
				Name:        "node-b",
				PodCIDRs:    []netip.Prefix{pfx("2001:db8:b::/64")},
				InternalIPs: []netip.Addr{addr("203.0.113.2"), addr("2001:db8::2")},
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("2001:db8:b::/64"), Via: addr("2001:db8::2")},
			},
		},
		{
			name: "this node's own CIDRs are never routed",
			nodes: []Node{self, {
				Name:        "node-b",
				PodCIDRs:    []netip.Prefix{pfx("198.51.100.0/24")},
				InternalIPs: []netip.Addr{addr("203.0.113.2")},
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("198.51.100.0/24"), Via: addr("203.0.113.2")},
			},
		},
		{
			name: "with several InternalIPs of one family the first listed is the next hop",
			nodes: []Node{self, {
				Name:        "node-b",
				PodCIDRs:    []netip.Prefix{pfx("198.51.100.0/24")},
				InternalIPs: []netip.Addr{addr("203.0.113.2"), addr("203.0.113.22")},
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("198.51.100.0/24"), Via: addr("203.0.113.2")},
			},
		},
		{
			// The kernel stores the masked prefix. If the set kept the host bits, a
			// later apply would not recognise its own route and remove it as stale.
			name: "a pod CIDR with host bits set is normalised",
			nodes: []Node{self, {
				Name:        "node-b",
				PodCIDRs:    []netip.Prefix{pfx("198.51.100.5/24"), pfx("2001:db8:b::1/64")},
				InternalIPs: []netip.Addr{addr("203.0.113.2"), addr("2001:db8::2")},
			}},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("198.51.100.0/24"), Via: addr("203.0.113.2")},
				{Node: "node-b", Dst: pfx("2001:db8:b::/64"), Via: addr("2001:db8::2")},
			},
		},
		{
			name: "a peer with no pod CIDR yet contributes nothing",
			nodes: []Node{self, {
				Name:        "node-b",
				InternalIPs: []netip.Addr{addr("203.0.113.2")},
			}},
		},
		{
			name: "the output is ordered by node name so that two runs compare equal",
			nodes: []Node{
				self,
				{Name: "node-c", PodCIDRs: []netip.Prefix{pfx("203.0.113.128/25")}, InternalIPs: []netip.Addr{addr("203.0.113.3")}},
				{Name: "node-b", PodCIDRs: []netip.Prefix{pfx("198.51.100.0/24")}, InternalIPs: []netip.Addr{addr("203.0.113.2")}},
			},
			wantRoutes: []Route{
				{Node: "node-b", Dst: pfx("198.51.100.0/24"), Via: addr("203.0.113.2")},
				{Node: "node-c", Dst: pfx("203.0.113.128/25"), Via: addr("203.0.113.3")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRoutes, gotMissing := Compute("node-a", tc.nodes)
			if diff := cmp.Diff(tc.wantRoutes, gotRoutes, cmp.Comparer(prefixEq), cmp.Comparer(addrEq)); diff != "" {
				t.Errorf("routes differ (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantMissing, gotMissing, cmp.Comparer(prefixEq)); diff != "" {
				t.Errorf("missing families differ (-want +got):\n%s", diff)
			}
		})
	}
}

func prefixEq(a, b netip.Prefix) bool { return a == b }
func addrEq(a, b netip.Addr) bool     { return a == b }

// The warning is repeated on every reconcile (ADR 0006), so it has to read well on
// its own: node, family, and what was not installed.
func TestMissingString(t *testing.T) {
	m := Missing{Node: "node-b", Family: "IPv6", PodCIDR: pfx("2001:db8:b::/64")}
	got := m.String()
	for _, want := range []string{"node-b", "IPv6", "2001:db8:b::/64", "InternalIP"} {
		if !contains(got, want) {
			t.Errorf("%q does not mention %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
