# ADR 0004: NodePolicy is a cluster-scoped CRD that is permissive by default and carries rules that cannot lock a node out

- Status: Accepted (2026-09-19). Revised the same day: permissive mode replaces the
  commit-confirmed apply that the first version of this record chose.

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
remove, and how an operator finds out what a policy would break before it breaks it.

## Decision

### Shape

`NodePolicy`, cluster-scoped, in API group `cnidaria.unstable.cloud`, version
`v1alpha1`. The group sits under a domain the project controls, as regied's does, and
the name of the binary does not appear in it so that the API does not move when the
binary is renamed.

| Field | Meaning |
|---|---|
| `spec.mode` | `Permissive` (the default) or `Enforce`. In `Permissive` the policy is rendered in full but nothing is dropped: what would have been dropped is logged and counted instead. As with SELinux, enforcing is chosen explicitly |
| `spec.nodeSelector` | A label selector over Nodes. Empty selects every node |
| `spec.policyTypes` | `Ingress`, `Egress` or both, with the same default rule as NetworkPolicy: `Ingress` always, `Egress` when egress rules are present |
| `spec.ingress[]` | Each entry: `from[]` peers and `ports[]`. A node selected by a policy with type `Ingress` accepts on `input` only what an entry allows |
| `spec.egress[]` | Each entry: `to[]` peers and `ports[]`. Same, on `output` |
| peer | `ipBlock` with `cidr` and `except`. Pod and namespace selectors are not peers here: a node is addressed by the network, not by the cluster |
| port | `protocol`, `port`, `endPort`, as in NetworkPolicy |
| `status.nodes[]` | Per selected node: `observedGeneration`, the `mode` that generation was applied in, and a `message` when it could not be rendered |

The vocabulary is NetworkPolicy's on purpose. An operator who can write one can write
the other, and the same renderer patterns (peer sets, port rules, "any policy allows")
apply. As with NetworkPolicy, a node selected by no policy of a given type is open in
that direction; a node selected by one is closed except for what is listed. Several
policies selecting one node are unioned, and a node is enforcing for a direction only
when every policy that selects it for that direction is in `Enforce`: one permissive
policy keeps the node observing.

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
and if they forget, permissive mode is what shows it.

Accepting these unconditionally is a deliberate coarseness for a first version: an
operator cannot restrict SSH to a management range through a NodePolicy. Narrowing a
safe rule by source is a later change to this record, not a reason to start without
them.

### Permissive by default

A NodePolicy is rendered the same way in both modes: the safe rules, then the dispatch
into the policy chains, then the verdict for a packet no rule accepted. Only that final
verdict differs.

| Mode | Final verdict in the dispatch chain |
|---|---|
| `Permissive` | `limit rate` → `log prefix "cnidaria-nodepolicy "` → `counter` → fall through to the base chain's `policy accept` |
| `Enforce` | `counter` → `drop` |

In `Permissive` a packet that the policy would deny is accepted, its source, destination,
protocol and port appear in the kernel log (and so in journald on the node) with the
prefix above, and the counter on the rule keeps the total. The rate limit keeps a busy
node from flooding its log; the counter is not rate-limited, so the count is exact even
when the log is sampled.

The way to bring a policy into service is therefore:

1. Apply it in `Permissive`, which is what an object without `spec.mode` gets.
2. Watch the log and the counter for as long as the traffic pattern needs — a full day
   for a node whose backups run at night.
3. Add the entries the log shows are missing, and set `spec.mode: Enforce`.

The two layers guard against different mistakes. **The safe rules** are for what a node
must never lose whatever the policy says; they hold in both modes and need no operator
action. **Permissive mode** is for everything else the operator forgot — a metrics
scraper, a memberlist port, a backup target — which cannot be enumerated in advance and
which the node can survive losing, but should not lose by surprise. The first makes a
policy unable to lock a node out; the second makes its whole effect visible before it
has any.

Because nothing about the ruleset changes between the modes except one verdict, what
was observed in `Permissive` is exactly what `Enforce` will do. There is no second
rendering to trust.

### Considered and not taken: commit-confirmed apply

The first version of this record chose a commit-confirmed apply: every change to the
node rules was provisional, the daemon opened a fresh connection to the API server
through the new ruleset, and a failure to reach it within a timer rolled the previous
ruleset back and marked the generation as failed. On start-up, a leftover table with an
unreachable API server was rewritten to the safe rules alone.

It was replaced because of what it costs against what it proves. The mechanism is a
timer, a dedicated probe connection, a remembered last-good ruleset, per-generation
memory of what failed, and a start-up path that undoes a previous run — a state machine
whose own bugs would sit in the code path that runs during an outage. And all of it
proves one thing: that the API server is reachable. It says nothing about SSH, about a
metrics scraper, or about the backup target. Permissive mode has no state machine at
all — it swaps one verdict — and it shows the whole effect of a policy, not the one
effect a probe can check.

If an `Enforce` apply ever causes a lockout in practice despite the safe rules and the
permissive step, this record is where that decision is revisited.

### Generated, not hand-written

The Go types carry controller-gen markers; the CRD manifest under `deploy/crd` and the
DeepCopy methods are generated from them (ADR 0007). The manifest is committed so that
`kustomize build` needs no tooling.

## Consequences

- The daemon needs no memory across applies for node policy: each reconcile renders
  the ruleset from the objects it sees, in whichever mode they declare, and applies it
  (ADR 0003). Start-up is the same as any other reconcile.
- Ingress from pods to the node's own sockets (a pod reaching a hostNetwork service, or
  metrics scraped from the node) is governed by NodePolicy like any other source, using
  the pod CIDRs in `ipBlock`. Pod selectors as node-policy peers can be added when a
  concrete need appears; leaving them out keeps the first version's peers static and
  the safe-rule reasoning simple.
- Status on a cluster-scoped object is written by every selected node. The status
  subresource and a per-node entry keep those writes from colliding.
- A policy left in `Permissive` protects nothing. That is visible in `status.nodes[]`
  and in the object itself, and it is the intended default: an operator has to choose
  to enforce.
- The log prefix is part of the interface. Tools that read the journal for it should
  expect the prefix to stay and the fields to be those nftables `log` emits.
- The same `mode` on NetworkPolicy — observing what a pod policy would drop before it
  drops it — is a natural later addition and is outside this task.
- The netns testbed (ADR 0008) asserts, for a selected node, that an unlisted port is
  logged and counted but reachable in `Permissive`, rejected in `Enforce`, and that
  kubelet-style and API-server-style connections survive in both.
