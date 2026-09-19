# ADR 0004: NodePolicy is a cluster-scoped CRD with rules that cannot lock a node out

- Status: Accepted (2026-09-19)

## Context

NetworkPolicy governs pods only. Nothing in the Kubernetes API describes what may reach
the node's own sockets — kubelet, SSH, the API server on a control-plane node — so
cnidaria adds a CRD for it, rendered into the `input` and `output` chains of ADR 0003.

This is the part of the project most able to cause an outage. A policy applied to a
control-plane node that forgets port 6443 severs the cluster from itself; one that
forgets 10250 makes every node `NotReady`; one that forgets 22 costs a trip to the
console. The first apply of a rule that drops by default happens on a live cluster, on
every selected node at once.

Three things have to be decided: the shape of the object, which rules a policy cannot
remove, and what happens when an apply turns out to be wrong.

## Decision

### Shape

`NodePolicy`, cluster-scoped, in API group `cnidaria.unstable.cloud`, version
`v1alpha1`. The group sits under a domain the project controls, as regied's does, and
the name of the binary does not appear in it so that the API does not move when the
binary is renamed.

| Field | Meaning |
|---|---|
| `spec.nodeSelector` | A label selector over Nodes. Empty selects every node |
| `spec.policyTypes` | `Ingress`, `Egress` or both, with the same default rule as NetworkPolicy: `Ingress` always, `Egress` when egress rules are present |
| `spec.ingress[]` | Each entry: `from[]` peers and `ports[]`. A node selected by a policy with type `Ingress` accepts on `input` only what an entry allows |
| `spec.egress[]` | Each entry: `to[]` peers and `ports[]`. Same, on `output` |
| peer | `ipBlock` with `cidr` and `except`. Pod and namespace selectors are not peers here: a node is addressed by the network, not by the cluster |
| port | `protocol`, `port`, `endPort`, as in NetworkPolicy |
| `status.nodes[]` | Per selected node: `observedGeneration` and whether that generation is `Applied`, `RolledBack` (with a message), or `Pending` |

The vocabulary is NetworkPolicy's on purpose. An operator who can write one can write
the other, and the same renderer patterns (peer sets, port rules, "any policy allows")
apply. As with NetworkPolicy, a node selected by no policy of a given type is open in
that direction; a node selected by one is closed except for what is listed. Several
policies selecting one node are unioned.

Ports and the API server address that the safe rules need are arguments to the daemon
with defaults matching the cluster's conventions (NodePort range `30000-32767`, API
server `6443`, kubelet `10250`, etcd `2379-2380`, SSH `22`).

### Safe rules that a policy cannot remove

Every `input` and `output` chain opens with the rules below, before any policy is
consulted, whether or not a NodePolicy exists. They are not a policy and no field turns
them off.

| Direction | Rule | Why |
|---|---|---|
| both | `ct state established,related accept` | Replies to what the node opened; ICMP errors tied to a connection |
| both | loopback interface accept | A host that cannot talk to itself is broken in a way no policy intends |
| both | ICMP: echo-request, destination-unreachable, time-exceeded, parameter-problem; ICMPv6: the same plus packet-too-big, router and neighbour solicitation and advertisement, and the MLD listener types | Path MTU discovery, neighbour discovery and address resolution. Without ND an IPv6 node vanishes from its own segment |
| input | TCP `22` | Console-free recovery |
| input | TCP `10250` | kubelet: without it the node is `NotReady` and `exec` / `logs` fail |
| input | TCP `6443` | The API server on a control-plane node |
| input | TCP `2379-2380` | etcd client and peer ports on a control-plane node |
| input | TCP and UDP NodePort range | Services exposed on the node; on most nodes DNAT moves this to `forward`, but a hostNetwork endpoint is reached through `input` |
| output | TCP `6443` to the API server | The daemon's own connection, kubelet's and kube-proxy's |
| output | TCP `2379-2380` | etcd peers and the API server's client connection |
| output | TCP `10250` | The API server reaching kubelets on other nodes |
| output | TCP and UDP `53` | Name resolution |
| output | this node's own pod CIDRs | kubelet probes and `exec` into local pods |

MetalLB in L2 mode is not on the list because it does not need to be: ARP is not IP and
never reaches an `inet` table, IPv6 neighbour discovery is covered by the ICMPv6 rule,
and traffic to a load balancer address is DNATed by kube-proxy and goes through
`forward`. Its speakers' memberlist port is an ordinary thing for the operator to allow,
and if they forget, the mechanism below is what catches it.

Accepting these unconditionally is a deliberate coarseness for a first version: an
operator cannot restrict SSH to a management range through a NodePolicy. Narrowing a
safe rule by source is a later change to this record, not a reason to start without
them.

### Commit-confirmed apply

Every apply that changes the node rules is provisional until the API server has been
reached through the new ruleset.

1. The daemon keeps the last confirmed table text.
2. It applies the new table and starts a timer (30 seconds by default).
3. It makes a request for its own Node object over a fresh connection. The connection
   is deliberately new so that a `ct state established` rule cannot make a broken policy
   look fine.
4. On success before the timer expires, the new text becomes the last confirmed one and
   status for this node reads `Applied`.
5. Otherwise it reapplies the last confirmed text, records `RolledBack` with the reason
   on the policy's status once the API server is reachable again, and does not try that
   generation of the policy again. A new generation (an edit) starts over.

The check is coarse — it proves the API server is reachable, not that SSH is — which is
why the safe rules exist as well. The two guard different mistakes: the safe rules
cover the ports a node must never lose, the confirmation covers the case where the
operator's own egress rule breaks the daemon's ability to receive the fix.

At start-up, the daemon applies nothing for node policy until it has listed the API
once. If an `inet cnidaria` table from an earlier run exists and the API server cannot
be reached within the confirmation window, the daemon rewrites that table's `input` and
`output` chains to hold only the safe rules, then keeps trying. A daemon restarted onto
a node it locked out on its previous run therefore opens the door itself.

### Generated, not hand-written

The Go types carry controller-gen markers; the CRD manifest under `deploy/crd` and the
DeepCopy methods are generated from them (ADR 0007). The manifest is committed so that
`kustomize build` needs no tooling.

## Consequences

- Ingress from pods to the node's own sockets (a pod reaching a hostNetwork service, or
  metrics scraped from the node) is governed by NodePolicy like any other source, using
  the pod CIDRs in `ipBlock`. Pod selectors as node-policy peers can be added when a
  concrete need appears; leaving them out keeps the first version's peers static and
  the safe-rule reasoning simple.
- Status on a cluster-scoped object is written by every selected node. The status
  subresource and a per-node entry keep those writes from colliding.
- A policy of type `Egress` on a node whose operator forgot NTP or a package mirror
  breaks those quietly. That is the ordinary consequence of default-deny and is what the
  `Egress` type opting in is for.
- The netns testbed (ADR 0008) asserts both halves: that a selected node rejects an
  unlisted port, and that kubelet-style and API-server-style connections survive.
