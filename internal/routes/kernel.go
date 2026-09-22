package routes

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
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

// GlobalIPv6 returns a global IPv6 address of the interface that holds on, which is
// the node's IPv4 InternalIP: the address the node publishes for its peers to route
// its IPv6 pod CIDR through when it has no IPv6 InternalIP (ADR 0006). It reports
// false when that interface has none, or no interface holds on.
func (k *Kernel) GlobalIPv6(on netip.Addr) (netip.Addr, bool, error) {
	v4, err := k.h.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return netip.Addr{}, false, fmt.Errorf("routes: list addresses: %w", err)
	}
	i := slices.IndexFunc(v4, func(a netlink.Addr) bool {
		held, ok := netip.AddrFromSlice(a.IP)
		return ok && held.Unmap() == on
	})
	if i < 0 {
		return netip.Addr{}, false, nil
	}
	link, err := k.h.LinkByIndex(v4[i].LinkIndex)
	if err != nil {
		return netip.Addr{}, false, fmt.Errorf("routes: link of %s: %w", on, err)
	}
	v6, err := k.h.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return netip.Addr{}, false, fmt.Errorf("routes: list addresses: %w", err)
	}
	ip, ok := pickGlobalIPv6(v6)
	return ip, ok, nil
}

// pickGlobalIPv6 picks the lowest IPv6 address of global scope that has settled and
// is meant to stay: not tentative or failed duplicate detection, not deprecated, and
// not a temporary privacy address that is replaced every so often. The lowest, so that
// the choice does not depend on the order the kernel lists them in.
func pickGlobalIPv6(addrs []netlink.Addr) (netip.Addr, bool) {
	const unusable = unix.IFA_F_TENTATIVE | unix.IFA_F_DADFAILED | unix.IFA_F_DEPRECATED | unix.IFA_F_TEMPORARY
	var best netip.Addr
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok || !ip.Is6() || ip.Is4In6() || a.Scope != unix.RT_SCOPE_UNIVERSE || a.Flags&unusable != 0 {
			continue
		}
		if !best.IsValid() || ip.Less(best) {
			best = ip
		}
	}
	return best, best.IsValid()
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
