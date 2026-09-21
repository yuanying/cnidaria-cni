# ADR 0005: Address allocation is host-local's, fed from `node.spec.podCIDRs`

- Status: Accepted (2026-09-19)
- Amended by: ADR 0009 (2026-09-19, corrected 2026-09-21) — the network name, and with
  it the store directory, is a setting (`--network-name`), so that a node migrating
  from another CNI can share its leases

## Context

Each pod needs one IPv4 and one IPv6 address out of the node's two `podCIDRs`, and the
allocation has to survive a daemon restart and a node reboot without handing out an
address that is still in use. The controller manager assigns `podCIDRs` when a Node
joins and never changes them, so the ranges are fixed inputs; what has to be decided is
who keeps the lease state.

The options were an allocator inside the daemon (the container runtime would then have
to ask it for an address, which means a plugin of our own — ruled out by ADR 0001), an
allocator inside a plugin of our own (same), and the reference `host-local` plugin.

## Decision

**IPAM is delegated to `host-local`.** cnidaria writes no allocator.

- The conflist's `bridge` entry names `host-local` as its IPAM, with one range set per
  address family, each holding that family's `podCIDR`. host-local allocates one address
  from each set, which is what gives a pod one address per family.
- `host-local` reserves the first address of each range for the gateway, which is the
  address `bridge` puts on `cni0` when `isDefaultGateway` is set. Nothing in cnidaria
  has to know the gateway address.
- Lease state is host-local's file store, `/var/lib/cni/networks/<network name>` on the
  node, one file per allocated address holding the container ID. It persists across
  daemon restarts and node reboots. The DaemonSet mounts nothing for it; the plugin runs
  on the host, not in the pod.
- The network name in the conflist is `cnidaria` unless the daemon is told otherwise
  (ADR 0009). It does not change while a node runs, so the store directory is stable
  and a conflist rewrite does not orphan leases.

## Consequences

- `podCIDRs` are the only input. A cluster whose controller manager allocates IPv4 only
  gets IPv4-only pods and an IPv4-only conflist; the renderer refuses two prefixes of one
  family because host-local's gateway convention assumes one range set per family.
- A pod whose `DEL` was never delivered (a runtime crash between sandbox teardown and
  the CNI call) leaves a stale lease file until something reclaims it. The reference
  plugins implement the CNI 1.1 `GC` verb for this; the container runtime in use does
  not yet call it. This is the same behaviour flannel-based nodes had, and it is
  bounded: one address per lost `DEL`, and a `/24` on a node that runs tens of pods has
  room. If it ever matters, the daemon can invoke `GC` with the list of live pod sandbox
  IDs, which is a later ADR.
- Changing `podCIDRs` on a Node is not supported by Kubernetes and not by this design.
  A node that must move ranges is drained, deleted and re-joined, which also clears the
  lease store.
