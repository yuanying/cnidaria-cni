# ADR 0003: One nftables table, `inet cnidaria`, and the order its chains run in

- Status: Accepted (2026-09-19)

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
inserted into anybody else's chain.

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
No other table is read, written or flushed; kube-proxy's are not touched.

The ruleset is a pure function of what the daemon has seen from the API server plus
this node's identity. Rendering runs no command and reads no kernel state.

### Five base chains

| Chain | Hook | Priority | Holds |
|---|---|---|---|
| `egress` | forward | `filter` (0) | NetworkPolicy egress, keyed on the source pod |
| `ingress` | forward | `filter + 1` | NetworkPolicy ingress, keyed on the destination pod |
| `input` | input | `filter` (0) | NodePolicy ingress with the safe rules ahead of it (ADR 0004) |
| `output` | output | `filter` (0) | NodePolicy egress with the safe rules ahead of it (ADR 0004) |
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
   peer pod sets, namespace pod sets, `ipBlock` ranges (with `except` as a set with
   intervals removed) and ports, each ending in `accept`. A policy chain that matches
   nothing returns.
5. The dispatch chain ends in `drop`. An isolated pod for which no policy accepted is
   denied.

"Allowed if any policy allows" falls out of `accept` being final for the base chain and
`return` falling through to the next policy.

Pods become named sets of addresses, one per family, so a pod coming or going changes
set contents and not chain structure. Rules carry `counter` and a `comment` with the
namespace and name they came from, so `nft list ruleset` reads in the operator's own
vocabulary. Names become nftables identifiers deterministically; a name that cannot be
made into one is a render error.

### Traffic from the node to its own pods is not pod policy

kubelet probes and `exec` sessions originate on the node and reach a local pod through
`output`, not `forward`. NetworkPolicy implementations conventionally let the node reach
its own pods, and a cluster whose health checks are subject to user policy is a cluster
that goes unhealthy for reasons that read as policy bugs. The `output` chain carries
NodePolicy only.

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
