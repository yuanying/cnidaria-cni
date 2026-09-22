// Package testbed builds the topology the netns tests run against (ADR 0008): one
// "segment" namespace standing in for the L2 network, "node" namespaces attached to
// it through veth pairs and carrying the sysctls a real node has (ADR 0002), and
// "pod" namespaces joined to a node's bridge by the real reference plugins driven by
// the real conflist (ADR 0001).
//
// Everything here needs root, ip, and the plugins under /opt/cni/bin (or
// CNIDARIA_CNI_PATH). The package has no build tag of its own so that vet and the
// linter always see it; only the tests that call it are tagged.
package testbed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// Names inside a node namespace. A pod's default gateway is the address the bridge
// plugin puts on PodBridge.
const (
	SegmentBridge = "seg0"
	NodeUplink    = "eth0"
	PodBridge     = "cni0"
)

// Segment is the shared L2 network every node hangs off.
type Segment struct {
	id string // a short random tag that keeps two concurrent runs apart
	NS string
}

// NewSegment creates the segment namespace with its bridge, loads br_netfilter if the
// host does not have it yet, and tears everything down when the test ends.
func NewSegment(t testing.TB) *Segment {
	t.Helper()
	requireBrNetfilter(t)

	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	s := &Segment{id: hex.EncodeToString(b)}
	s.NS = s.name("seg")
	addNS(t, s.NS)
	mustRun(t, "ip", "netns", "exec", s.NS, "ip", "link", "add", SegmentBridge, "type", "bridge")
	mustRun(t, "ip", "netns", "exec", s.NS, "ip", "link", "set", SegmentBridge, "up")
	return s
}

func (s *Segment) name(parts ...string) string {
	return "cn" + s.id + "-" + strings.Join(parts, "-")
}

// NodeSpec is what a Node object would say about a node: its pod CIDRs and its
// InternalIPs. The prefix length on an InternalIP is the segment's, so that the
// addresses of the other nodes are on-link. OtherIPs are addresses the uplink holds
// that the Node object does not report, such as an IPv6 address on a node whose
// kubelet reports only its IPv4 one.
type NodeSpec struct {
	Name        string
	PodCIDRs    []netip.Prefix
	InternalIPs []netip.Prefix
	OtherIPs    []netip.Prefix
}

// AddNode creates a node namespace joined to the segment: uplink veth, InternalIPs,
// forwarding and br_netfilter sysctls. The bridge itself appears when the first pod
// is added, as it does on a real node.
func (s *Segment) AddNode(t testing.TB, spec NodeSpec) *Node {
	t.Helper()
	n := &Node{
		Name:     spec.Name,
		NS:       s.name(spec.Name),
		PodCIDRs: spec.PodCIDRs,
		segment:  s,
		cacheDir: t.TempDir(), // outlives any subtest that adds the first pod
		podNames: map[string]bool{},
	}
	for _, p := range spec.InternalIPs {
		n.InternalIPs = append(n.InternalIPs, p.Addr())
	}
	addNS(t, n.NS)

	segSide := spec.Name + s.id // must fit IFNAMSIZ; node names are short
	mustRun(t, "ip", "link", "add", segSide, "netns", s.NS, "type", "veth", "peer", "name", NodeUplink, "netns", n.NS)
	mustRun(t, "ip", "netns", "exec", s.NS, "ip", "link", "set", segSide, "master", SegmentBridge, "up")
	n.Exec(t, "ip", "link", "set", "lo", "up")
	n.Exec(t, "ip", "link", "set", NodeUplink, "up")
	for _, p := range append(slices.Clone(spec.InternalIPs), spec.OtherIPs...) {
		if p.Addr().Is6() {
			n.Exec(t, "ip", "-6", "addr", "add", p.String(), "dev", NodeUplink, "nodad")
		} else {
			n.Exec(t, "ip", "addr", "add", p.String(), "dev", NodeUplink)
		}
	}
	// The settings a real node has once the daemon runs: br_netfilter from the
	// node's provisioning, forwarding turned on by the daemon (ADR 0002).
	n.Exec(t, "sysctl", "-q", "-w",
		"net.ipv4.ip_forward=1",
		"net.ipv6.conf.all.forwarding=1",
		"net.bridge.bridge-nf-call-iptables=1",
		"net.bridge.bridge-nf-call-ip6tables=1")
	return n
}

// Run executes a command in the segment namespace.
func (s *Segment) Run(t testing.TB, args ...string) string {
	t.Helper()
	return mustRun(t, append([]string{"ip", "netns", "exec", s.NS}, args...)...)
}

// requireBrNetfilter loads the module when the bridge sysctls are absent. It goes
// into the host kernel, which is why the test container is privileged and mounts
// /lib/modules.
func requireBrNetfilter(t testing.TB) {
	t.Helper()
	if _, err := os.Stat("/proc/sys/net/bridge/bridge-nf-call-iptables"); err == nil {
		return
	}
	mustRun(t, "modprobe", "br_netfilter")
}

func addNS(t testing.TB, name string) {
	t.Helper()
	mustRun(t, "ip", "netns", "add", name)
	t.Cleanup(func() {
		if out, err := run(context.Background(), "ip", "netns", "del", name); err != nil {
			t.Logf("ip netns del %s: %v\n%s", name, err, out)
		}
	})
}

// mustRun runs a command on the host side and fails the test if it fails.
func mustRun(t testing.TB, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), args...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	return string(out), err
}

// Eventually retries probe until it returns nil or the timeout passes. Address
// settling and neighbour discovery take a moment after a namespace comes up, so a
// reachability check is retried; an unreachability check is not.
func Eventually(t testing.TB, timeout time.Duration, what string, probe func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := probe()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s: %v", what, timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
