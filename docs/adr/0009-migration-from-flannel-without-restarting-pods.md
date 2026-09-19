# ADR 0009: Migrating from flannel host-gw in place, without restarting pods

- Status: Accepted (2026-09-19)
- Amends: ADR 0005 (the ipam store name is no longer tied to the network name)

## Context

The clusters cnidaria is for run flannel with the `host-gw` backend today. That data
plane has the same shape cnidaria keeps on purpose (ADR 0001): one Linux bridge named
`cni0` per node created by the reference `bridge` plugin, addresses handed out by
`host-local`, one route per peer node with the peer's address as next hop, and the
`portmap` plugin for hostPort. flannel's part in it is a small CNI plugin that reads the
node's subnet from a file, builds the `bridge` + `host-local` configuration at ADD time
and delegates to it, plus a daemon that writes that file and the routes.

The question is whether cnidaria can take over a node while its pods keep running, or
whether every pod has to be recreated. Restarting every pod in a cluster is a
maintenance window; a takeover that keeps them is not. The pieces below are the ones
that decide it. Each was checked against what the runtime and the plugins actually do,
not against what would be convenient.

## Decision

**cnidaria takes over a flannel host-gw node in place. Running pods keep their
addresses and their connectivity.** What makes that true, and what the design commits
to because of it:

### The same bridge

cnidaria's conflist names the bridge `cni0`, the name flannel's delegate configuration
produces by default. The pods that exist are attached to that bridge, its gateway
address is on it, and the kernel's connected route for the pod CIDR points at it. New
pods created through cnidaria's conflist join the same bridge and see the old ones as
neighbours.

A bridge of another name is not an option. The `bridge` plugin would create it, put the
same gateway address on it, and the node would then hold two interfaces in one subnet:
two connected routes for one prefix, and the kernel picking one of them for every
packet. The running pods on the old bridge would lose their gateway.

### Routes are replaced by destination

Every route cnidaria installs is written with a replace, keyed on the destination
prefix (ADR 0006). flannel's route to a peer's pod CIDR has the same destination, so
cnidaria's write replaces it in one netlink operation. There is no moment at which the
prefix has no route, and no moment at which two routes compete. After the first
reconcile every peer route carries cnidaria's protocol marker, and from then on they
are owned and reclaimed as ADR 0006 says. A flannel route to a node that had already
left the cluster before the takeover is not replaced, because nothing computes it; it
is a leftover for the runbook to name, since cnidaria does not delete routes it did
not mark.

### The address store is shared by name

`host-local` keeps its leases under `/var/lib/cni/networks/<name>`. The name is the
network name unless the ipam block carries its own `name`, in which case that is used
instead. flannel's conflist is named `cbr0`, so the leases of every running pod are
under `cbr0`. If cnidaria allocated from a store of its own, the first new pod would
be handed an address that a running pod already holds.

cnidaria's conflist therefore can name the ipam store separately from the network:
the daemon's `--ipam-store-name` flag puts that name in the ipam block, and a node
migrating from flannel is started with `cbr0`. The default is empty, which leaves
host-local on the network name, as ADR 0005 described. The network name itself stays
`cnidaria`, since the runtime keys its own bookkeeping by it.

A store named after a previous CNI is a permanent setting on such a cluster, not a
transitional one: the leases written after the takeover live in the same directory,
and renaming it later would orphan them. This is the only visible trace the migration
leaves.

### flannel's plugin stays until its pods are gone

The container runtime records the configuration it used for a pod's ADD and calls DEL
with that same configuration, not with whatever the directory holds at DEL time. For a
pod created under flannel that configuration names the `flannel` plugin, which at DEL
reads the delegate configuration it saved for that container under
`/var/lib/cni/flannel` and hands the DEL on to `bridge` and `host-local`. Removing the
`flannel` binary or that directory while such pods exist makes every one of their
DELs fail: the sandbox is torn down anyway, but the lease is never released and the
runtime logs an error per pod.

So `/opt/cni/bin/flannel` and `/var/lib/cni/flannel` are left in place until the last
pod created under flannel has been deleted. cnidaria neither needs them nor removes
them; the runbook says when they can go.

### Which conflist the runtime picks

The runtime reads `/etc/cni/net.d` and uses the first file in name order. cnidaria
writes `10-cnidaria.conflist`, which sorts before `10-flannel.conflist`. From the
moment the daemon has written its file, every new pod is created through cnidaria's
plugins, whether or not flannel's file is still there.

cnidaria does not delete `10-flannel.conflist`, or any file it did not write. Removing
it is a step in the migration runbook. The ordering makes that step safe to do late:
nothing depends on it except tidiness.

### Masquerade

flannel's `--ip-masq` rules in iptables and cnidaria's masquerade in `inet cnidaria`
(ADR 0003) both translate the same traffic. Both being present for the length of a
rollout is harmless: a packet is translated once by whichever hook sees it first, and
both translate to the same node address. The flannel rules disappear with flannel's
DaemonSet.

## Consequences

- The migration is a rollout, not a window. Its steps, their order and how to verify
  each one are a runbook and not this record; the runbook is written by the deployment
  unit against the decisions here.
- ADR 0005's statement that the store directory follows the fixed network name is
  amended: the store follows the ipam name when one is set. The rest of ADR 0005 stands.
- The daemon's manifests must be able to carry `--ipam-store-name=cbr0` on a migrated
  cluster and omit it on a fresh one. A cluster that starts on cnidaria never sets it.
- The netns testbed (ADR 0008) names the ipam store per node for its own reasons, and
  in doing so exercises the same field on the real plugin.
- A future CNI that replaces cnidaria has the same three things to preserve: the
  bridge name, the ipam store name, and the previous plugin binary until its pods are
  gone. This record is the checklist.
