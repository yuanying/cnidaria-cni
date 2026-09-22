package testbed

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/invoke"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netns"

	"github.com/yuanying/cnidaria-cni/internal/conflist"
	"github.com/yuanying/cnidaria-cni/internal/routes"
)

// PluginDir is where the reference plugins are looked for. CNIDARIA_CNI_PATH
// overrides it.
func PluginDir() string {
	if p := os.Getenv("CNIDARIA_CNI_PATH"); p != "" {
		return p
	}
	return "/opt/cni/bin"
}

// Node is one "node": a namespace with an uplink to the segment and, once a pod has
// been added, the bridge the pods sit on.
type Node struct {
	Name        string
	NS          string
	PodCIDRs    []netip.Prefix
	InternalIPs []netip.Addr

	segment  *Segment
	cacheDir string // libcni's result cache; DEL reads what ADD stored here
	cni      *libcni.CNIConfig
	list     *libcni.NetworkConfigList
	podNames map[string]bool
}

// Exec runs a command in the node namespace and fails the test if it fails.
func (n *Node) Exec(t testing.TB, args ...string) string {
	t.Helper()
	return mustRun(t, append([]string{"ip", "netns", "exec", n.NS}, args...)...)
}

// Try runs a command in the node namespace and returns its outcome.
func (n *Node) Try(args ...string) (string, error) {
	return run(context.Background(), append([]string{"ip", "netns", "exec", n.NS}, args...)...)
}

// RoutesNode is this node as the route computation sees it.
func (n *Node) RoutesNode() routes.Node {
	return routes.Node{Name: n.Name, PodCIDRs: n.PodCIDRs, InternalIPs: n.InternalIPs}
}

// Kernel returns the route applier for this node's namespace.
func (n *Node) Kernel(t testing.TB) *routes.Kernel {
	t.Helper()
	ns, err := netns.GetFromName(n.NS)
	if err != nil {
		t.Fatalf("netns %s: %v", n.NS, err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	k, err := routes.NewKernelAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(k.Close)
	return k
}

// Conflist renders this node's list exactly as the daemon would, except that the
// network is named per node and run: every node's plugin shares one filesystem here,
// host-local keeps its leases under the network name, and it must not hand the same
// lease to two nodes.
func (n *Node) Conflist(t testing.TB) []byte {
	t.Helper()
	data, err := conflist.Render(conflist.Params{
		Name:     n.NS,
		Bridge:   PodBridge,
		MTU:      1500,
		PodCIDRs: n.PodCIDRs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Pod is a namespace attached to a node's bridge by the plugins.
type Pod struct {
	Name string
	NS   string
	// IPs are the addresses host-local assigned, one per family.
	IPs []netip.Addr
	// HostVeth is the node-side end of the pod's veth, the bridge port.
	HostVeth string
	node     *Node
}

// AddPod creates a pod namespace and runs CNI ADD with the node's conflist through the
// real plugins, executed inside the node namespace so that the bridge and the veth
// land there. CNI DEL runs at cleanup so host-local's leases do not accumulate.
func (n *Node) AddPod(t testing.TB, name string) *Pod {
	t.Helper()
	if n.podNames[name] {
		t.Fatalf("node %s already has a pod named %s", n.Name, name)
	}
	n.podNames[name] = true
	if n.cni == nil {
		n.cni = libcni.NewCNIConfigWithCacheDir([]string{PluginDir()}, n.cacheDir, &nsExec{ns: n.NS})
		list, err := libcni.ConfListFromBytes(n.Conflist(t))
		if err != nil {
			t.Fatalf("conflist: %v", err)
		}
		n.list = list
	}

	p := &Pod{Name: name, NS: n.segment.name(n.Name, name), node: n}
	addNS(t, p.NS)
	p.Exec(t, "ip", "link", "set", "lo", "up")

	rt := &libcni.RuntimeConf{
		ContainerID: p.NS,
		NetNS:       filepath.Join("/var/run/netns", p.NS),
		IfName:      "eth0",
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := n.cni.AddNetworkList(ctx, n.list, rt)
	if err != nil {
		t.Fatalf("CNI ADD for pod %s on node %s: %v", name, n.Name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := n.cni.DelNetworkList(ctx, n.list, rt); err != nil {
			t.Logf("CNI DEL for pod %s on node %s: %v", name, n.Name, err)
		}
	})

	result, err := types100.NewResultFromResult(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range result.IPs {
		addr, ok := netip.AddrFromSlice(ip.Address.IP)
		if !ok {
			t.Fatalf("CNI result holds an unreadable address %v", ip.Address)
		}
		p.IPs = append(p.IPs, addr.Unmap())
	}
	for _, iface := range result.Interfaces {
		// The host-side veth is the interface without a sandbox; the bridge has
		// none either, so its name is excluded by name.
		if iface.Sandbox == "" && iface.Name != PodBridge {
			p.HostVeth = iface.Name
		}
	}
	if len(p.IPs) == 0 || p.HostVeth == "" {
		t.Fatalf("CNI result for pod %s lacks addresses or the host veth: %+v", name, result)
	}
	return p
}

// Exec runs a command in the pod namespace and fails the test if it fails.
func (p *Pod) Exec(t testing.TB, args ...string) string {
	t.Helper()
	return mustRun(t, append([]string{"ip", "netns", "exec", p.NS}, args...)...)
}

// IP returns the pod's address of the family, or fails.
func (p *Pod) IP(t testing.TB, v6 bool) netip.Addr {
	t.Helper()
	for _, a := range p.IPs {
		if a.Is6() == v6 {
			return a
		}
	}
	t.Fatalf("pod %s has no address of that family: %v", p.Name, p.IPs)
	return netip.Addr{}
}

// Ping sends one echo request from the pod and reports whether a reply came back
// within a second. ping picks the family from the address.
func (p *Pod) Ping(addr netip.Addr) error {
	out, err := run(context.Background(), "ip", "netns", "exec", p.NS, "ping", "-c", "1", "-W", "1", addr.String())
	if err != nil {
		return fmt.Errorf("ping %s from pod %s: %w\n%s", addr, p.Name, err, strings.TrimSpace(out))
	}
	return nil
}

// nsExec runs each plugin inside the node namespace. libcni otherwise runs it in
// the process's own namespace, which is where a real container runtime lives, but
// here every "node" is a namespace of its own.
type nsExec struct {
	invoke.DefaultExec
	ns string
}

func (e *nsExec) ExecPlugin(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, "ip", "netns", "exec", e.ns, pluginPath)
	c.Env = environ
	c.Stdin = bytes.NewReader(stdinData)
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("%s in %s: %w\nstdout: %s\nstderr: %s", filepath.Base(pluginPath), e.ns, err,
			strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// FlushNeighbours empties the pod's neighbour cache, so that the next packet to
// another pod starts with address resolution.
func (p *Pod) FlushNeighbours(t testing.TB) {
	t.Helper()
	p.Exec(t, "ip", "neigh", "flush", "all")
	p.Exec(t, "ip", "-6", "neigh", "flush", "all")
}
