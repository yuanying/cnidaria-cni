# Architecture decision records

Decisions are recorded before the code that depends on them is written. A record is
not edited to say something else once accepted; a change of mind is a new record that
supersedes it, or an amendment that says what changed and when. Japanese versions are
under [docs/ja/adr](../ja/adr/README.md).

| # | Title | Decides |
|---|---|---|
| [0001](0001-delegate-the-data-plane-to-the-reference-plugins.md) | Delegate the data plane to the reference CNI plugins | No CNI binary of our own; the daemon writes a conflist naming `bridge`, `host-local`, `portmap`; masquerade is ours |
| [0002](0002-same-node-pod-traffic-passes-through-br-netfilter.md) | Same-node pod traffic passes through br_netfilter | The sysctls are assumed and checked at start-up; the daemon refuses to start without them |
| [0003](0003-one-nftables-table-and-the-order-of-chains.md) | One nftables table and the order of chains | `inet cnidaria`, five base chains, egress and ingress as separate base chains, `nft -f` atomic replacement |
| [0004](0004-node-policy-crd-and-lockout-prevention.md) | NodePolicy CRD and lockout prevention | Cluster-scoped CRD shaped like NetworkPolicy, safe rules a policy cannot remove, permissive by default with `Enforce` opted into |
| [0005](0005-ipam-is-host-local.md) | IPAM is host-local | Ranges from `node.spec.podCIDRs`, state in host-local's file store |
| [0006](0006-next-hop-per-address-family.md) | Next hop per address family | InternalIP of the same family; a missing family is logged, not worked around; routes carry an ownership marker |
| [0007](0007-controller-runtime-for-everything-the-daemon-watches.md) | controller-runtime for everything the daemon watches | One manager, two reconcilers, controller-gen for the CRD, no kubebuilder scaffold |
| [0008](0008-tests-split-by-privilege.md) | Tests split by privilege | Unit tests need the toolchain only; netns tests behind a build tag, run in a privileged container |
