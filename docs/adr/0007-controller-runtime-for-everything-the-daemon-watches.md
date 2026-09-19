# ADR 0007: The daemon watches the API through controller-runtime, without the kubebuilder scaffold

- Status: Accepted (2026-09-19)

## Context

The daemon reacts to five kinds of object: Nodes (routes, masquerade set, the conflist),
Pods and Namespaces (which addresses a selector names), NetworkPolicies, and its own
NodePolicy CRD. All of them feed one output per node — the routing table and one
nftables table — so what is wanted is a cache of those objects and a signal that
something changed, not per-object bookkeeping.

Three ways of getting that were compared.

| Option | Core types | CRD | What it costs |
|---|---|---|---|
| client-go informers everywhere | `SharedInformerFactory` | Hand-rolled REST client and scheme registration, or code-generator's clientset / listers / informers | Two mechanisms unless code-generator is used; code-generator adds a generation pipeline and several thousand generated lines |
| controller-runtime | Manager cache | Same cache; the type is registered in the scheme | One dependency tree; one way to read anything |
| controller-runtime via the kubebuilder scaffold | as above | as above | A `PROJECT` file, a prescribed tree (`api/`, `controllers/`, `config/`), Makefile and Dockerfile conventions that assume a webhook-and-operator product |

The project's standing instruction is that code and architecture be easy for a person
to read and that indirection be added only when it is needed now.

## Decision

**controller-runtime, used as a library, for every watch — core types and the CRD
alike — and none of the kubebuilder scaffolding.**

- A single `Manager` with the default cache. The cache is client-go informers
  underneath, so nothing is lost against option one, and every object is read the same
  way through the manager's client.
- The CRD's Go types live in `internal/apis/v1alpha1` with controller-gen markers.
  `make generate` produces the DeepCopy methods and the CRD manifest. controller-gen is
  a tool in `go.mod`'s `tool` directive and runs through `go tool`; nothing is installed
  globally. code-generator is not used.
- Two reconcilers, both small, both recomputing from the cache rather than tracking
  deltas:

  | Reconciler | Triggered by | Does |
  |---|---|---|
  | routes | Node add / update / delete | Recomputes the full route set (ADR 0006); on this node's own object, writes the conflist once (ADR 0001) |
  | ruleset | Pod, Namespace, NetworkPolicy, NodePolicy, Node | Renders and applies `inet cnidaria` (ADR 0003, 0004) |

  Every event for the ruleset reconciler maps to one fixed request key, so the
  work queue collapses a burst of pod events into one reconcile. That is the debounce
  ADR 0003 asks for, and it is the queue's ordinary behaviour, not a mechanism of ours.
- No leader election (each node acts for itself only), no webhooks, no metrics or
  health endpoints beyond what the DaemonSet's probes need, bound to localhost.
- Memory on a small control-plane node is kept down by a cache transform that drops
  `managedFields` and other bulk from every cached object, and by watching Pods with
  only the fields the renderer reads. The Pod cache is cluster-wide by necessity: a
  NetworkPolicy peer on this node can name a pod on any other.
- Package layout is this repository's own: `internal/controller` holds the reconcilers,
  the domain packages (`routes`, `netpol`, `nodepol`, `nftables`, `conflist`) know
  nothing about controller-runtime and are testable without a cluster.

## Consequences

- A reader who knows controller-runtime finds the familiar `Reconcile` shape; a reader
  who does not finds two functions and a manager, and no generated framework around
  them.
- controller-runtime's version pins the `k8s.io` module versions. They are chosen to be
  compatible with the cluster version in use and moved together.
- Status updates on NodePolicy (ADR 0004) go through the manager's client with the
  status subresource, the same client as every read.
- If a later need for per-object reconciliation appears — say, a per-NodePolicy
  condition that needs its own retry — it is a third small reconciler, not a change to
  the two here.
