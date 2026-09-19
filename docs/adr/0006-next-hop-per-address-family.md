# ADR 0006: One route per peer node and family, with that node's InternalIP as next hop

- Status: Accepted (2026-09-19)

## Context

The `host-gw` data plane is one route per peer node: this node reaches the other node's
pod CIDR by sending to the other node's address on the shared L2 segment, and the other
node's kernel forwards onto its bridge. With two address families that is up to two
routes per peer. The inputs are on the Node object: `spec.podCIDRs` and the
`InternalIP` entries in `status.addresses`, of which there may be one per family, or one.

What has to be decided is which address is the next hop for each family and what to do
when a family is missing on one side.

## Decision

### Per family, from InternalIP

For each peer node and each family, the daemon installs
`<peer podCIDR of that family> via <peer InternalIP of that family>` when both exist,
and nothing for that family when either is missing.

- The next hop is the peer's `InternalIP` of the same family. A route's gateway must be
  on-link, and a node's InternalIP is its address on the cluster segment by definition;
  a link-local address is not what the Node object reports and would need an interface
  to be chosen alongside it.
- When a node has more than one InternalIP of a family, the first listed is used, which
  is the order kubelet reports and the one everything else in the cluster uses.
- The kernel resolves the outgoing interface from the gateway. If the gateway is not
  on-link — a node on a different segment — the route install fails, the failure is
  logged with the node's name, and nothing else changes: `host-gw` is for one segment
  and a node outside it is not reachable by this design.

### A missing family is visible, not worked around

A peer that has a pod CIDR of one family but no InternalIP of that family gets no route
for that family. The daemon logs it at warning level with the node name and the missing
family, and repeats the warning on every reconcile that finds it unchanged, so it does
not scroll off. It does not:

- fall back to the other family's InternalIP (an IPv4 gateway cannot carry an IPv6 route
  and the reverse is equally meaningless);
- encapsulate (there is no overlay in this design);
- remove the working family's route to make the failure symmetric.

The asymmetry the task warns about — this node has both families, the peer has one —
is exactly this case. The rule is: install what can be installed, and say what could
not. Pods on this node reach the peer's pods over the family both sides have and cannot
reach them over the other; the peer's daemon, running the same rule, reaches this node
the same way. That is a half-connected pair, and the warning is what tells the operator
which node to fix. A design that silently made it look whole would be doing so by
dropping the family on every node.

### Ownership

Routes are installed with a routing protocol number reserved for cnidaria, and only
routes carrying it are reclaimed. The reconcile computes the full set the node should
have, adds what is missing, replaces what differs, and deletes those with the marker
that are not in the set. Routes installed by anyone else — the kernel's connected
routes, an operator's static routes, a routing daemon's — are never listed as
candidates for removal, as regied's
[ADR 0009](https://github.com/yuanying/regied/blob/main/docs/adr/0009-ownership-boundary.md)
requires.

A route removed behind the daemon's back — by an operator, or by an interface going
down and taking its routes with it — is put back by the next reconcile. One runs on
every Node event and, so that nothing waits on an event, on a timer as well.

Routes are written through a netlink library rather than by running `ip`, because a
route is a small fixed struct with nothing to render and nothing an operator would
diff, unlike a ruleset (ADR 0003).

### The node's own CIDRs

The node's own pod CIDRs need no route from the daemon: the `bridge` plugin puts the
gateway address on `cni0`, and the kernel's connected route for that prefix follows.

## Consequences

- The daemon needs `list` and `watch` on Nodes and nothing else for routing; it never
  writes a Node.
- The route set and the masquerade set of ADR 0003 are derived from the same Node
  objects in the same reconcile, so the two cannot disagree about which nodes exist.
- A node that changes its InternalIP (a re-addressed segment) gets its routes replaced
  on the next reconcile, since the next hop is part of what is computed.
- The netns testbed (ADR 0008) covers the missing-family case by giving one "node" an
  IPv4 InternalIP only and asserting IPv4 reachability, IPv6 unreachability, and the
  presence of the warning.
