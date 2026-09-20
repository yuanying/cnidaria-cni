package routes

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Protocol is the routing protocol number every route cnidaria installs carries, and
// the only thing that makes a route eligible for removal by cnidaria. It is
// unassigned in iproute2's rt_protos table and shows up as "proto 200" in
// "ip route show".
const Protocol netlink.RouteProtocol = 200

// Kernel installs a route set through netlink into one network namespace: the
// daemon's own on a node, a "node" namespace in the netns tests.
type Kernel struct {
	h *netlink.Handle
}

// NewKernel talks to the namespace the process runs in.
func NewKernel() (*Kernel, error) {
	h, err := netlink.NewHandle()
	if err != nil {
		return nil, fmt.Errorf("routes: netlink: %w", err)
	}
	return &Kernel{h: h}, nil
}

// NewKernelAt talks to the given namespace instead.
func NewKernelAt(ns netns.NsHandle) (*Kernel, error) {
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, fmt.Errorf("routes: netlink: %w", err)
	}
	return &Kernel{h: h}, nil
}

// Close releases the netlink socket.
func (k *Kernel) Close() { k.h.Close() }

// Apply makes the main routing table hold exactly want among the routes marked with
// Protocol: each wanted route is added or replaced, and every marked route whose
// destination is not wanted is deleted. Routes without the marker are never
// candidates for anything (ADR 0006).
//
// A route that cannot be installed, typically because its gateway is not on-link,
// does not stop the others. Every failure is returned, each naming its node.
func (k *Kernel) Apply(want []Route) error {
	existing, err := k.h.RouteListFiltered(netlink.FAMILY_ALL,
		&netlink.Route{Protocol: Protocol}, netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return fmt.Errorf("routes: list: %w", err)
	}

	var errs []error
	wanted := make(map[netip.Prefix]bool, len(want))
	for _, r := range want {
		wanted[r.Dst] = true
		if err := k.h.RouteReplace(toNetlink(r)); err != nil {
			errs = append(errs, fmt.Errorf("routes: install %s: %w", r, err))
		}
	}
	for i := range existing {
		dst, ok := prefixOf(existing[i].Dst)
		if !ok || wanted[dst] {
			continue
		}
		if err := k.h.RouteDel(&existing[i]); err != nil {
			errs = append(errs, fmt.Errorf("routes: remove stale %s: %w", dst, err))
		}
	}
	return errors.Join(errs...)
}

func toNetlink(r Route) *netlink.Route {
	return &netlink.Route{
		Dst: &net.IPNet{
			IP:   r.Dst.Masked().Addr().AsSlice(),
			Mask: net.CIDRMask(r.Dst.Bits(), r.Dst.Addr().BitLen()),
		},
		Gw:       r.Via.AsSlice(),
		Protocol: Protocol,
	}
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	addr, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(addr.Unmap(), ones), true
}
