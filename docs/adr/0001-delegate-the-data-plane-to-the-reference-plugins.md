# ADR 0001: Delegate the data plane to the reference CNI plugins, and ship no plugin binary of our own

- Status: Accepted (2026-09-19)

## Context

cnidaria replaces flannel's `host-gw` backend on a cluster whose nodes all sit on one
L2 segment: one Linux bridge per node, pods addressed out of the node's
`node.spec.podCIDRs` (IPv4 and IPv6), and a route per peer node in the host routing
table. What flannel does not do, and cnidaria adds, is enforce NetworkPolicy and a
policy for the node itself.

The question is how much of the per-pod work — creating the veth pair, attaching it to
the bridge, assigning addresses, setting up hairpin and hostPort — cnidaria writes
itself. Three shapes were compared.

**A. Write the CNI plugin.** Own binary in `/opt/cni/bin` implementing ADD / DEL / CHECK.
Full control, and a full copy of what `bridge` already does: link creation, netns
juggling, gateway addresses, IPv6 DAD, hairpin, the CHECK verb, GC in CNI 1.1.

**B. A thin wrapper, as flannel does.** Own binary that reads per-node subnet
information from a file the daemon writes, builds a `bridge` + `host-local`
configuration at ADD time and delegates to it. This is the shape flannel has, and it
exists for a reason that does not apply here: flannel's conflist is a static file from
a ConfigMap, identical on every node, so something has to inject the node's subnet at
run time. The wrapper is that something.

**C. No binary. The daemon writes the whole conflist.** The node daemon already knows
its own `podCIDRs`, so it writes a per-node conflist naming `bridge`, `host-local` and
`portmap` directly, with the ranges filled in. The container runtime invokes the
reference plugins and nothing of ours runs at pod creation time.

## Decision

**C.** cnidaria ships no CNI binary. The daemon renders a conflist that names the
reference plugins and writes it to `/etc/cni/net.d`; the reference plugins do all
per-pod work.

The conflist is the one below, with the node's CIDRs in the ranges.

| Plugin | Settings | Why |
|---|---|---|
| `bridge` | `bridge: cni0`, `isDefaultGateway: true`, `hairpinMode: true`, `mtu` from the node's uplink | The same delegate settings flannel used, so a pod sees the same network it did |
| `host-local` (as `bridge`'s IPAM) | one range set per address family from `node.spec.podCIDRs` | ADR 0005 |
| `portmap` | `capabilities.portMappings: true` | hostPort keeps working, as it did under flannel |

The list declares CNI `1.0.0`, which the container runtime in use and the reference
plugins both speak.

### Why not A

Everything A would write is a re-implementation of `bridge`, which regied's
[ADR 0008](https://github.com/yuanying/regied/blob/main/docs/adr/0008-delegate-to-existing-implementations.md)
calls owning a layer somebody else already owns. The bridge plugin is maintained, tested
against every CNI version, and handles the corners (IPv6 DAD, hairpin versus promiscuous
mode, CHECK, GC) that a fresh implementation gets wrong for a year. None of cnidaria's
value is in that layer.

### Why not B

B keeps a binary only to move a few numbers from the daemon to the plugin at ADD time.
Since the daemon writes the conflist per node anyway, the numbers can go in the conflist
and the binary disappears. What is left is one fewer thing to build for two
architectures, one fewer process on the pod-creation path, and one fewer place where a
version of ours has to match a version of theirs.

B would be the right shape again only if the per-node input changed while pods run.
`node.spec.podCIDRs` is set once by the controller manager and never changes for the
life of a Node object, so it does not.

### What this leaves cnidaria owning

| Layer | Owner |
|---|---|
| veth, bridge port, addresses, hairpin, hostPort | reference plugins |
| Address allocation and its state | `host-local` (ADR 0005) |
| Which conflist a node gets | cnidaria daemon |
| Routes to peer nodes' pod CIDRs | cnidaria daemon (ADR 0006) |
| Source NAT for pod traffic leaving the cluster network | cnidaria daemon (ADR 0003) |
| NetworkPolicy and NodeNetworkPolicy enforcement | cnidaria daemon (ADR 0003, 0004) |
| Service load balancing | kube-proxy, untouched |

Masquerade needs a note. Under flannel the `--ip-masq` flag installed the rule that
translates pod traffic bound outside the cluster network to the node's address. Without
it, a pod's packets to anything outside the pod CIDRs leave with a pod source address
that the surrounding network cannot route back. The `bridge` plugin has an `ipMasq`
option, but it writes iptables rules per pod from inside the plugin, which would put a
second writer of iptables-nft state next to kube-proxy's and tie the plugin to an
iptables binary on the host. The masquerade rule is therefore cnidaria's, in its own
table, keyed on the cluster's pod CIDRs (ADR 0003).

## Consequences

- The task's original goal statement listed "a CNI plugin binary". That item is gone; the
  deliverable is the conflist and the daemon. `make build` builds one binary.
- The reference plugins have to be on every node. The daemon image carries them and the
  DaemonSet's init container copies them into `/opt/cni/bin`, so a node needs nothing
  pre-installed beyond a container runtime.
- Pod creation never runs cnidaria code. If the daemon is down, pods still start and get
  addresses; what stops being maintained is routes and policy, and a policy that is not
  yet rendered means traffic to a newly started pod is not filtered until the daemon
  catches up. This is the same failure window every controller-based enforcer has.
- The netns tests (ADR 0008) exercise the real plugins through the real conflist, not a
  mock of them.
