# ADR 0009: Migrating from flannel host-gw in place, without restarting pods

- Status: Accepted (2026-09-19), amended (2026-09-21, twice)
- Amends: ADR 0005 (the network name, which names the ipam store, is a setting)

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

### The address store is shared through the network name

`host-local` keeps its leases under `/var/lib/cni/networks/<network name>`. The ipam
block has a `name` field of its own, but host-local overwrites it with the network name
before it opens the store, so the network name alone decides the directory. flannel's
conflist is named `cbr0`, so the leases of every running pod are under `cbr0`. If
cnidaria allocated from a store of its own, the first new pod would be handed an
address that a running pod already holds.

cnidaria's network name is therefore a setting: the daemon's `--network-name` flag,
`cnidaria` by default, and a node migrating from flannel is started with `cbr0`. The
bridge and everything else in the conflist are the same whatever the name.

A network named after a previous CNI is a permanent setting on such a cluster, not a
transitional one: the leases written after the takeover live in the same directory,
and renaming it later would orphan them. This is the only visible trace the migration
leaves.

### The previous CNI's configuration leaves before cnidaria arrives

A container runtime does not necessarily use only the first file in `/etc/cni/net.d`.
containerd loads as many as its `max_conf_num` setting allows, and when it has loaded
more than one, a new pod is attached to every one of them: two interfaces, two
addresses, two default routes. With `10-cnidaria.conflist` and `10-flannel.conflist`
side by side, a pod created in between would be attached to both.

So the migration takes the previous CNI out of the directory before cnidaria writes
into it, in this order: delete flannel's DaemonSet, so that nothing writes its
conflist again; delete `10-flannel.conflist` on every node; then deploy cnidaria. The
running pods are untouched throughout, since the bridge, their addresses and the routes
stay where they are in the kernel. A pod created while no conflist is present waits in
`ContainerCreating`, because the runtime reports the network as not ready, and is set
up as soon as cnidaria has written its file.

cnidaria does not delete `10-flannel.conflist`, or any file it did not write. Removing
it is a step of the migration, done by the operator.

### Pods created under flannel are deleted through cnidaria's conflist

containerd calls DEL with the configuration in the directory at the time of the DEL,
not with the one it used for the pod's ADD. A pod created under flannel is therefore
torn down by cnidaria's `bridge` and `host-local`. That works because the network name
is the same (see above): host-local releases the lease it finds in the shared store
under that container's ID, and `bridge` removes the pod's veth from the same `cni0`.

flannel's binary under `/opt/cni/bin` and its state under `/var/lib/cni/flannel` are
then no longer read. Leaving them in place does no harm, and cnidaria does not remove
them.

### Masquerade

flannel's `--ip-masq` rules in iptables and cnidaria's masquerade in `inet cnidaria`
(ADR 0003) both translate the same traffic. Both being present is harmless: a packet
is translated once by whichever hook sees it first, and both translate to the same node
address. flannel does not remove all of its rules when its DaemonSet is deleted; its
IPv6 rules have been seen to stay behind. They are removed by hand, at leisure.

## Consequences

- The migration is a rollout, not a window. Its steps, their order and how to verify
  each one are a runbook and not this record; the runbook is written by the deployment
  unit against the decisions here.
- ADR 0005's statement that the network name is fixed is amended: it is `cnidaria`
  unless the daemon is told otherwise. The rest of ADR 0005 stands.
- The daemon's manifests must be able to carry `--network-name=cbr0` on a migrated
  cluster and omit it on a fresh one. A cluster that starts on cnidaria never sets it.
- The netns testbed (ADR 0008) names the network per node for its own reasons, and in
  doing so exercises the same store naming on the real plugin.
- New pods cannot be created between the removal of flannel's conflist and the arrival
  of cnidaria's on a node. That gap is as short as the rollout makes it, and nothing
  running is affected.
- A future CNI that replaces cnidaria has the same things to preserve: the bridge name,
  the network name, and cnidaria's conflist gone from the directory before its own
  arrives. This record is the checklist.

## Amendments

**2026-09-21, when the migration was tried on a running cluster.** The first version
said the ipam block's own `name` chose host-local's store, and added an
`--ipam-store-name` flag that put the previous CNI's name there while the network kept
the name `cnidaria`. host-local does not honour that field: it replaces it with the
network name, so the flag changed nothing and the store stayed a new one. The flag is
gone. The network name is now the setting (`--network-name`), which is what
host-local actually reads, and the section on the address store says so.

**2026-09-21, from the same migration.** Three statements about the runtime and
flannel were wrong, and the sections that depended on them have been rewritten:

- "The runtime uses the first file in name order." containerd loads up to
  `max_conf_num` files and attaches a pod to every network it loaded. cnidaria's file
  sorting first was supposed to make the removal of flannel's file a tidy-up that could
  wait; it cannot, and the migration now removes the previous CNI's configuration
  before cnidaria is deployed.
- "The runtime calls DEL with the configuration it used for ADD." containerd uses the
  configuration present at DEL time. Keeping flannel's binary and state until its pods
  were gone was the consequence of that statement; it is no longer needed, and what
  releases an old pod's lease is the shared network name.
- "flannel's masquerade rules disappear with its DaemonSet." Its IPv6 rules stayed.
  They are harmless and are removed by hand.
