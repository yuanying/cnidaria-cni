# ADR 0002: Same-node pod traffic is filtered through br_netfilter, which the cluster already requires

- Status: Accepted (2026-09-19), amended (2026-09-21): the daemon turns on IP forwarding
  instead of requiring it.

## Context

Two pods on the same node sit on the same bridge and the same subnet. A frame between
them is switched at L2 by the bridge and, by default, never enters the IP stack of the
host: no `prerouting`, no `forward`, no `postrouting`. A NetworkPolicy rendered into IP
layer hooks would then apply to traffic between nodes and be blind to traffic between
neighbours on one bridge — the worst kind of policy, one that appears to work.

The kernel's answer is the `br_netfilter` module with the sysctls
`net.bridge.bridge-nf-call-iptables` and `net.bridge.bridge-nf-call-ip6tables` set to
`1`. With them, bridged IPv4 and IPv6 frames are passed through the IP layer netfilter
hooks as if they were routed, so `forward` sees them and conntrack tracks them.

This is not a new demand on the node. kube-proxy in iptables mode needs the same thing
for the same reason: a pod talking to a Service is DNATed in `prerouting` to another pod
that may be on the same bridge, and the reply from that pod has to be un-DNATed. If the
reply is switched at L2 without entering netfilter, the client sees a reply from an
address it never sent to, and the connection fails. Because of this:

- The Kubernetes documentation for the version this targets (v1.29) lists the two sysctls,
  together with loading `br_netfilter`, under "Forwarding IPv4 and letting iptables see
  bridged traffic" on the container runtime prerequisites page.
- kube-proxy's iptables proxier reads `bridge-nf-call-iptables` at start-up and logs
  that the proxy "may not work as intended" when it is not `1`.
- kubeadm's preflight checks in the same version read
  `/proc/sys/net/bridge/bridge-nf-call-iptables` (and the `ip6tables` twin when IPv6 is in
  use) and expect `1`.

Every node this is meant for was provisioned to satisfy kube-proxy, so the sysctls are
already set.

Two other places could see same-node traffic without br_netfilter and were considered.

- **The bridge family of nftables** (`table bridge`) hooks on the bridge's own path. It
  sees every frame, but conntrack is not available there, so "established" cannot be
  expressed and every reply would need its own rule. It would also mean a second table
  with a second copy of every policy.
- **A veth per pod without a bridge** (each pod on its own /32 and /128, routed by the
  host) makes every packet a routed packet. That is a different data plane from the
  flannel `host-gw` one this project keeps on purpose (ADR 0001).

## Decision

**cnidaria requires br_netfilter with both sysctls set to 1, and refuses to start when
they are not. IP forwarding it turns on itself.**

- The daemon reads `net.bridge.bridge-nf-call-iptables` and
  `net.bridge.bridge-nf-call-ip6tables` at start-up. A value other than `1`, or the files
  being absent because the module is not loaded, is a fatal error whose message names the
  sysctl and the value it wants.
- The daemon also reads `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding`, which a
  node that routes for its pods needs in any case, and writes `1` to whichever is not
  `1`, logging each one it changed. A setting it cannot write, because the file is
  read-only to it or it lacks the privilege, is a fatal error naming the sysctl and the
  file it tried to write.
- The daemon does not load modules or set the br_netfilter sysctls. They belong to node
  provisioning, where the cluster already sets them for kube-proxy; a daemon that quietly
  changes a setting some other component also depends on is harder to reason about than
  one that says what it needs. Forwarding is different in kind: it is the routing the
  CNI does, and the CNI turning it on is what nodes already expect (see Amendments).

Refusing to start over br_netfilter, rather than warning, is deliberate. A warning under
a stream of other log lines is how the policy silently does not apply to half the
traffic. A daemon that does not come up is noticed, and it is noticed before any policy
was ever trusted.

With br_netfilter in place, every packet between two pods — on one node or on two —
passes through the host's `forward` hook exactly once per node it crosses, and conntrack
sees both directions. That is what ADR 0003's chain layout relies on.

## Consequences

- The netns testbed (ADR 0008) has to set the same sysctls in its "node" namespaces,
  which the test harness does explicitly so that the assumption is written down where
  the tests are.
- Matching is on addresses, not on bridge port names: with br_netfilter a bridged frame
  in `forward` reports the bridge as both input and output interface, so interface
  matching cannot tell two pods apart. The pod sets in ADR 0003 are address sets.
- If a future kube-proxy stops needing br_netfilter, cnidaria still does. The start-up
  check is cnidaria's own for that reason and does not defer to anything kube-proxy
  reports.
- The container runtime mounts a container's `/proc/sys` read-only, a pod on the host's
  network included. The DaemonSet mounts the host's `/proc/sys/net` writable at
  `/host/proc/sys/net` and points the daemon at it with a flag; outside a container the
  flag's default is `/proc/sys`. How this was chosen is under Amendments.
- With forwarding at `1` the kernel treats the node as a router, and an interface with
  `accept_ra` at `1` stops accepting router advertisements; only `accept_ra` at `2`
  accepts them while forwarding. A node that takes its IPv6 default route from router
  advertisements therefore needs `accept_ra=2` on that interface, or a static default
  route, from its provisioning, or the route expires after cnidaria starts. cnidaria does
  not touch `accept_ra`: how a node accepts routes from its network is the node's own
  decision, and a CNI overriding it would surprise whoever made it.

## Amendments

**2026-09-21, after running on a cluster migrated from another CNI.** The first version
of this record required the two forwarding sysctls as well and left them to provisioning,
with the same refusal to start. A CNI that routes for its pods commonly turns forwarding
on at run time; flannel does, for IPv6 when it is enabled. Nodes provisioned beside such
a CNI need not persist the setting, and nothing on the node records that it was the CNI,
not provisioning, that turned it on. After the CNI is replaced, everything keeps working
until the node reboots: forwarding comes back off, the daemon refuses to start, and the
node has no pod network. Turning forwarding on is what the replaced CNI did, so cnidaria
doing the same means a migration does not change what a node needs. br_netfilter stays a
requirement: it is a kernel module, kube-proxy depends on it too, and provisioning sets it
for that reason.

Writing the setting from inside a pod needed a way past the read-only `/proc/sys`. Three
ways were considered.

- **The host's `/proc/sys/net` mounted writable at another path** (chosen). The files under
  `/proc/sys/net` answer for the network namespace of the process that opens them, so
  for a pod on the host's network they are the node's. The container stays unprivileged
  with the `NET_ADMIN` it already has, only the network settings become writable, not all
  of `/proc/sys`, and the daemon sets forwarding every time it starts, so a restart of
  the daemon alone puts it back. Tried with a container runtime before it was chosen: the
  default `/proc/sys` refuses the write and the mounted path accepts it.
- **A privileged init container running `sysctl -w`.** It leaves the daemon's container
  alone, but it is a privileged container where none was needed, and an init container
  does not run again when only the daemon's container restarts.
- **A privileged daemon container**, as flannel runs. The simplest, and the widest
  privilege for a single write.
