# ADR 0003: One nftables table, `inet cnidaria`, and the order its chains run in

- Status: Accepted (2026-09-19), amended (2026-09-20, 2026-09-21)

## Context

The node already carries a large amount of iptables-nft state written by kube-proxy:
`ip nat` and `ip filter` tables (and their `ip6` twins) with `PREROUTING`, `INPUT`,
`FORWARD`, `OUTPUT` and `POSTROUTING` base chains at the standard priorities. cnidaria
has to add NetworkPolicy enforcement for pods, a policy for the node itself (ADR 0004),
and the masquerade rule flannel used to own (ADR 0001) without disturbing any of it.

regied's
[ADR 0013](https://github.com/yuanying/regied/blob/main/docs/adr/0013-nftables-ruleset-shape.md)
is the shape to start from: one `inet` table, atomic replacement with `nft -f`, never
flush the ruleset, name things after what the operator wrote. Two facts about netfilter
decide the rest.

**A verdict in one base chain does not end evaluation at that hook.** Base chains at
the same hook — in one table or in several — run one after another in priority order.
`accept` ends the chain it is in; the packet still runs through the next base chain.
`drop` is final. So kube-proxy's `FORWARD` accepting a packet does not stop a cnidaria
chain from dropping it, and a cnidaria `accept` does not exempt a packet from
kube-proxy's rules. Nobody has to be first to be effective, and nothing has to be
inserted into anybody else's chain — except that, `drop` being final, a `FORWARD` whose
policy is `DROP` drops pod traffic whatever cnidaria accepts (see Amendments).

**DNAT happens in `prerouting` before `forward` runs.** kube-proxy's Service
translation is at priority `dstnat` (-100). By the time a `forward` chain at priority
`filter` (0) sees the packet, the destination is the pod that was picked. Policy is
therefore evaluated against pod addresses, which is what NetworkPolicy semantics
describe: an egress rule names the pods traffic may reach, not the Service in front of
them. Source NAT (`srcnat`, 100) runs after `forward`, so the source address a policy
sees is the original one for traffic on the way out and the masqueraded node address for
traffic that another node forwarded.

## Decision

### One table, `inet cnidaria`, replaced atomically

Everything cnidaria writes lives in `inet cnidaria`. It is applied with `nft -f` from a
file that adds the table, deletes it and declares it again in one transaction, so the
host never holds half a ruleset and a reapply of the same state is the same operation.
No other table is read, written or flushed; kube-proxy's are not touched. The one
exception, a chain of cnidaria's own and a jump to it in iptables' `FORWARD`, is under
Amendments.

The ruleset is a pure function of what the daemon has seen from the API server plus
this node's identity. Rendering runs no command and reads no kernel state.

### Five base chains

| Chain | Hook | Priority | Holds |
|---|---|---|---|
| `egress` | forward | `filter` (0) | NetworkPolicy egress, keyed on the source pod |
| `ingress` | forward | `filter + 1` | NetworkPolicy ingress, keyed on the destination pod |
| `input` | input | `filter` (0) | NodeNetworkPolicy ingress with the safe rules ahead of it (ADR 0004) |
| `output` | output | `filter` (0) | NodeNetworkPolicy egress with the safe rules ahead of it (ADR 0004) |
| `postrouting` | postrouting | `srcnat` (100) | Masquerade for pod traffic leaving the pod CIDRs |

Every base chain has `policy accept`. Denial is written as an explicit `drop` at the end
of a dispatch chain, so a table that is empty or half-rendered can only fail open, never
lock a node out, and "what is denied" is always a rule that can be read and counted.

Egress and ingress are two base chains, not two regular chains under one, because of
the first fact above. A packet from a pod with an egress policy to a pod with an
ingress policy on the same node must satisfy both. Inside one base chain the `accept`
that passes the egress check would end evaluation before the ingress check ran; as two
base chains, each check runs to its own verdict and the packet gets through only if
neither dropped it.

### Pod policy chains

The pattern in each of `egress` and `ingress`:

1. `ct state established,related accept`. NetworkPolicy governs who may open a
   connection; replies follow.
2. A jump into a dispatch chain for packets whose pod address is in the *isolated* set
   for that direction (one set per family). A pod is isolated for a direction when at
   least one NetworkPolicy with that `policyType` selects it. Unselected pods match no
   set and are not evaluated at all, which is the "all allowed" default.
3. In the dispatch chain, one rule per NetworkPolicy jumps into that policy's regular
   chain when the pod address is in that policy's selected-pods set.
4. A policy chain holds one rule per `from` / `to` entry and port combination, matching
   peer pod sets, namespace pod sets, `ipBlock` ranges and ports, each ending in
   `accept`. A policy chain that matches nothing returns.
5. The dispatch chain ends in `drop`. An isolated pod for which no policy accepted is
   denied.

"Allowed if any policy allows" falls out of `accept` being final for the base chain and
`return` falling through to the next policy.

An `ipBlock`'s `except` list is a second, negated match on the same address in the same
rule, rather than the ranges being subtracted from the set the rule reads. The two say
the same thing: either way the rule does not match an excepted address, and what this
rule does not match stays open to the next entry and the next policy. The choice is
which is simpler, and the negated match is. Subtracting the ranges means doing interval
arithmetic on prefixes while rendering; the negated match hands that to nft, which does
it already, and leaves a rule an operator can read the `except` list straight out of.

A port a policy names rather than numbers is resolved against whichever pods receive the
traffic — the pods the policy selects on ingress, the pods the entry names on egress —
and the rule carries the addresses of the pods that gave that name that number alongside
the number itself. Two pods may give one name two numbers, or one of them may not
declare it at all, so opening the number on every pod the rule covers would open a port
nobody asked for.

Pods become named sets of addresses, one per family, so a pod coming or going changes
set contents and not chain structure. Rules carry `counter` and a `comment` with the
namespace and name they came from, so `nft list ruleset` reads in the operator's own
vocabulary. Names become nftables identifiers deterministically. A name with a
character nft cannot read is a render error rather than text that would parse as
something else, but a name that is merely too long is not: a namespace and a name can
be 63 and 253 bytes, so nft's 255-byte identifier and 128-byte comment are both
reachable without anybody doing anything odd, and refusing them would stop the node's
whole table being updated until the object was renamed. An over-long identifier keeps
its head and ends in a hash of the whole of it, which is a function of the name alone,
so every node renders the same object to the same identifier. An over-long comment is
cut with an ellipsis, a comment being read rather than matched on.

### Traffic from the node to its own pods is not pod policy

kubelet probes and `exec` sessions originate on the node and reach a local pod through
`output`, not `forward`. NetworkPolicy implementations conventionally let the node reach
its own pods, and a cluster whose health checks are subject to user policy is a cluster
that goes unhealthy for reasons that read as policy bugs. The `output` chain carries
NodeNetworkPolicy only.

### Masquerade

`postrouting` translates traffic whose source is in this node's pod CIDRs and whose
destination is outside the cluster's pod CIDRs (all nodes' `podCIDRs`, kept in a set per
family) to the node's address on the outgoing link, with plain `masquerade` and no
flags, as regied's ADR 0013 argues. Pod-to-pod and pod-to-Service traffic is excluded by
the destination check because DNAT has already turned a Service address into a pod
address. kube-proxy's own `POSTROUTING` masquerades the packets it has marked; a
connection gets one source-NAT binding, set by whichever chain evaluates first, and
both chains map to the same node address, so the order between the two tables does not
matter.

### Applied with `nft -f`, not through a netlink library

The subprocess route is chosen over a Go netlink binding. The nft text is the native
language of the thing being configured: what the daemon writes is what `nft list table
inet cnidaria` shows and what an operator can paste back. It can be rendered without a
kernel, diffed in a test as a string, and inspected in a bug report. A binding would
need a build-time dependency on libnftnl or a large pure-Go serializer, and its output
would need `nft` to read anyway. The image carries the `nftables` package for this.

The whole table is replaced on every change, with changes debounced so that a burst of
pod events becomes one apply. At the scale this is for (hundreds of pods, tens of
policies) the file is a few hundred lines and the apply is milliseconds; if that stops
being true, sets can be updated incrementally without changing this layout.

## Consequences

- `ct state established,related accept` at the head of both pod chains means a
  NetworkPolicy change never cuts a running connection. This matches the behaviour of
  every widely used implementation and is what users expect; it also means a newly
  isolated pod keeps the connections it already had.
- Node policy and pod policy never interact through a shared chain. `input` / `output`
  govern what reaches the node's own sockets, `forward` governs pods, and a packet is
  evaluated by exactly one of the two groups.
- The masquerade rule needs the cluster's pod CIDRs, so the route reconciler
  (ADR 0006) and the ruleset renderer read the same Node objects. When a node joins, the
  route and the set entry arrive in the same reconcile.
- A future `nftables`-mode kube-proxy writes its own `ip kube-proxy` tables. Nothing in
  this layout depends on kube-proxy's table names or chain names, only on where DNAT
  happens, which is the same.

## Amendments

**2026-09-20, when the NetworkPolicy renderer was written.** Step 4 above said an
`ipBlock`'s `except` was "a set with intervals removed". It is a negated match instead.
Both mean the same thing; the negated match needs no interval arithmetic of ours and
reads back as what the operator wrote, which is the reason now given under the steps.

The same amendment added the paragraph on a port a policy names rather than numbers,
and replaced "a name that cannot be made into an identifier is a render error" with
what the renderer does about length. Neither reverses a decision: the first had not
been written down, and the second was found when a name at the API's own limits turned
out to make an identifier and a comment that nft refuses, taking the node's whole table
with them.

**2026-09-21, after running on a node with a container engine.** The owner boundary gets
one exception. A container engine, a host firewall or a distribution's default can set
the policy of iptables' `filter` `FORWARD` chain to `DROP`; a common container engine
does so whenever it starts. That chain is a base chain at the forward hook like
cnidaria's, and a `drop` is final, so every pod packet that no rule in `FORWARD` accepts
is dropped, whatever `inet cnidaria` does. The CNI this replaces had a rule of its own
in `FORWARD` that accepted pod traffic, and when it was removed, pod traffic between
nodes stopped. Nothing in `inet cnidaria` can undo that: an `accept` there ends only
its own chain.

So cnidaria keeps, for each family, a chain of its own in the `filter` table,
`CNIDARIA-FWD`, with one rule that accepts traffic from and one that accepts traffic to
each of the cluster's pod CIDRs, and one jump to it, with a comment naming cnidaria, at
the head of `FORWARD`. This is what the other CNIs that route for pods do, each with a
chain of its own in `FORWARD`.

- **What it touches.** The jump, and its own chain. The chain is replaced with
  `iptables-restore --noflush`, which empties and refills that one chain in one
  transaction, so there is no moment at which it is empty. The jump is looked for with
  `-C` and inserted first only when `-C` says it is missing (exit status 1); any other
  failure of `-C` is an error to retry, not a reason to insert a second jump. A jump
  that is there but no longer first is left where it is.
- **What it does not touch.** The policy of `FORWARD`, any other rule in it, and any other
  chain or table. Nothing is flushed but `CNIDARIA-FWD`.
- **When.** The route reconciler (ADR 0006) already reads every Node, so it applies the
  chain with the routes, on every Node event and every resync. A chain or jump removed
  behind the daemon's back, as a container engine does when it restarts and rewrites
  `FORWARD`, comes back within the resync interval.
- **Which iptables.** iptables has two backends, nft and legacy, and rules written through
  one are not seen by the other. For each family the daemon uses the one that holds
  more of kube-proxy's `KUBE-` chains, as kube-proxy's own image and other CNIs decide.
  A family with no `KUBE-` chains in either follows the other family's choice, since
  kube-proxy may run single-stack and a node uses one backend for both; with none in
  either family it is nft, or legacy where only legacy is installed. The choice is made
  at start-up and logged with its reason. Without iptables at all
  the daemon refuses to start; the image carries it. The legacy backend needs `NET_RAW`
  and the node's `/run/xtables.lock`, which the DaemonSet grants.
- **What it does not change.** Pod policy. The `accept` in `CNIDARIA-FWD` ends only that
  chain, like any other, so `inet cnidaria`'s `egress` and `ingress` still drop what a
  NetworkPolicy denies.
- **What is left behind.** Uninstalling cnidaria does not remove the chain or the jump.
  They accept only pod traffic and do nothing once there are no pods.

Two alternatives were rejected. Setting the policy of `FORWARD` to `ACCEPT` would override
a choice the node's owner, or another program, made on purpose. Requiring the node to
be provisioned so that pod traffic is accepted would leave the pod network one
container-engine restart away from going down, which is the failure this is for.
