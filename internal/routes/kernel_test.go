package routes

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func nlAddr(cidr string, scope, flags int) netlink.Addr {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	ipnet.IP = ip
	return netlink.Addr{IPNet: ipnet, Scope: scope, Flags: flags}
}

// The address a node publishes for its peers to route through has to be one that
// stays: global, settled, and not a privacy address that is replaced every so often.
func TestPickGlobalIPv6(t *testing.T) {
	cases := []struct {
		name  string
		addrs []netlink.Addr
		want  string
	}{
		{name: "nothing to pick"},
		{
			name: "IPv4 and link-local are skipped",
			addrs: []netlink.Addr{
				nlAddr("203.0.113.1/24", unix.RT_SCOPE_UNIVERSE, 0),
				nlAddr("fe80::1/64", unix.RT_SCOPE_LINK, 0),
				nlAddr("2001:db8::1/64", unix.RT_SCOPE_UNIVERSE, 0),
			},
			want: "2001:db8::1",
		},
		{
			name: "tentative, deprecated, failed and temporary addresses are skipped",
			addrs: []netlink.Addr{
				nlAddr("2001:db8::a/64", unix.RT_SCOPE_UNIVERSE, unix.IFA_F_TENTATIVE),
				nlAddr("2001:db8::b/64", unix.RT_SCOPE_UNIVERSE, unix.IFA_F_DEPRECATED),
				nlAddr("2001:db8::c/64", unix.RT_SCOPE_UNIVERSE, unix.IFA_F_DADFAILED),
				nlAddr("2001:db8::d/64", unix.RT_SCOPE_UNIVERSE, unix.IFA_F_TEMPORARY),
				nlAddr("2001:db8::e/64", unix.RT_SCOPE_UNIVERSE, unix.IFA_F_PERMANENT),
			},
			want: "2001:db8::e",
		},
		{
			name: "with several the lowest is picked, whatever order the kernel lists them in",
			addrs: []netlink.Addr{
				nlAddr("2001:db8::9/64", unix.RT_SCOPE_UNIVERSE, 0),
				nlAddr("2001:db8::3/64", unix.RT_SCOPE_UNIVERSE, 0),
			},
			want: "2001:db8::3",
		},
		{
			name:  "only unusable addresses leave nothing",
			addrs: []netlink.Addr{nlAddr("2001:db8::a/64", unix.RT_SCOPE_UNIVERSE, unix.IFA_F_TENTATIVE)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickGlobalIPv6(tc.addrs)
			if tc.want == "" {
				if ok {
					t.Errorf("picked %s, want nothing", got)
				}
				return
			}
			if !ok || got != addr(tc.want) {
				t.Errorf("picked %s (ok %v), want %s", got, ok, tc.want)
			}
		})
	}
}
