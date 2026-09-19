// Package routes keeps the host routing table pointing at the other nodes' pod
// CIDRs, one route per peer node and address family, with that node's InternalIP as
// the next hop (flannel host-gw style, ADR 0006). It owns only the routes carrying
// its protocol marker and never touches anyone else's.
//
// Compute is the decision and depends on nothing but its arguments; Kernel is the
// hands and talks to netlink. The controller calls one and then the other.
package routes

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Node is what the route computation needs to know about one Node object.
type Node struct {
	Name string
	// PodCIDRs is node.spec.podCIDRs: at most one prefix per family.
	PodCIDRs []netip.Prefix
	// InternalIPs are the InternalIP entries of node.status.addresses, in the order
	// kubelet reports them.
	InternalIPs []netip.Addr
}

// Route is one entry this node should have: a peer's pod CIDR reached through that
// peer's address on the shared segment. Node names the peer so that a failure to
// install the route can say which node it was for.
type Route struct {
	Node string
	Dst  netip.Prefix
	Via  netip.Addr
}

func (r Route) String() string {
	return fmt.Sprintf("%s via %s (node %s)", r.Dst, r.Via, r.Node)
}

// Missing records a peer that has a pod CIDR of one family but no InternalIP of that
// family. No route is installed for it; the operator is told instead (ADR 0006).
type Missing struct {
	Node    string
	Family  string // "IPv4" or "IPv6"
	PodCIDR netip.Prefix
}

func (m Missing) String() string {
	return fmt.Sprintf("node %s has the %s pod CIDR %s but no %s InternalIP; no %s route installed for it",
		m.Node, m.Family, m.PodCIDR, m.Family, m.Family)
}

// Compute returns the routes the node named self should have, given every Node in the
// cluster, and the families it could not route. The node's own CIDRs are excluded:
// the bridge plugin's gateway address gives the kernel a connected route for them.
// Destinations are masked, since that is the form the kernel reports them in and the
// form Kernel.Apply compares against. Output is sorted by node name and then prefix
// so that equal inputs compare equal.
func Compute(self string, nodes []Node) ([]Route, []Missing) {
	var routes []Route
	var missing []Missing
	for _, n := range nodes {
		if n.Name == self {
			continue
		}
		for _, cidr := range n.PodCIDRs {
			via, ok := firstOfFamily(n.InternalIPs, cidr.Addr().Is6())
			if !ok {
				missing = append(missing, Missing{Node: n.Name, Family: family(cidr.Addr()), PodCIDR: cidr})
				continue
			}
			routes = append(routes, Route{Node: n.Name, Dst: cidr.Masked(), Via: via})
		}
	}
	slices.SortFunc(routes, func(a, b Route) int { return byNodeThenPrefix(a.Node, a.Dst, b.Node, b.Dst) })
	slices.SortFunc(missing, func(a, b Missing) int { return byNodeThenPrefix(a.Node, a.PodCIDR, b.Node, b.PodCIDR) })
	return routes, missing
}

func byNodeThenPrefix(nodeA string, prefixA netip.Prefix, nodeB string, prefixB netip.Prefix) int {
	if c := strings.Compare(nodeA, nodeB); c != 0 {
		return c
	}
	return strings.Compare(prefixA.String(), prefixB.String())
}

// firstOfFamily picks the first listed address of the wanted family: with several
// InternalIPs of one family, the first is the one the rest of the cluster uses.
func firstOfFamily(addrs []netip.Addr, v6 bool) (netip.Addr, bool) {
	for _, a := range addrs {
		if a.Is6() == v6 {
			return a, true
		}
	}
	return netip.Addr{}, false
}

func family(a netip.Addr) string {
	if a.Is6() {
		return "IPv6"
	}
	return "IPv4"
}
