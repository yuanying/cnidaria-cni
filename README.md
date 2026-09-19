# cnidaria-cni

A Kubernetes CNI for small clusters whose nodes share one L2 segment: a bridge per
node, host routing between nodes (flannel `host-gw` style), IPv4 / IPv6 dual stack,
and — what flannel does not do — NetworkPolicy enforcement and a policy for the nodes
themselves, both in nftables.

The name is the phylum of jellyfish and anemones: it looks harmless until you touch
it.

日本語版は [README.ja.md](README.ja.md) を参照。

> **Status: design recorded, implementation in progress.** The decisions are in
> [docs/adr](docs/adr/README.md); the packages exist as a skeleton.

## What it does

- Writes a CNI conflist per node that names the reference plugins `bridge`,
  `host-local` and `portmap`, with the node's `podCIDRs` as the address ranges. Pods
  get one IPv4 and one IPv6 address, hairpin and hostPort work as they did.
- Keeps a route to every other node's pod CIDRs, per address family, with that node's
  InternalIP as the next hop.
- Renders NetworkPolicy v1 — podSelector, namespaceSelector, ipBlock, ports, endPort,
  policyTypes, "selected means isolated" — into one nftables table, `inet cnidaria`.
- Accepts a cluster-scoped `NodePolicy` for the node's own `input` and `output`, with
  safe rules a policy cannot remove. A policy is permissive by default: what it would
  drop is logged and counted until `Enforce` is chosen.
- Source-NATs pod traffic that leaves the cluster network.

## What it does not do

- **Replace kube-proxy.** Service load balancing stays where it is; cnidaria's table
  sits beside kube-proxy's and touches nothing in it.
- **Overlay networking.** Nodes must reach each other on one segment. There is no
  VXLAN, no tunnel, no BGP.
- **Its own CNI binary or IPAM.** The reference plugins do the per-pod work; cnidaria
  is a node daemon and a conflist.

## Layout

| Path | What is there |
|---|---|
| `cmd/cnidaria` | The node daemon |
| `internal/` | One package per responsibility; each `doc.go` says what it owns |
| `deploy/` | kustomize manifests: DaemonSet, RBAC, CRD |
| `docs/adr/` | Design decisions, with Japanese under `docs/ja/adr/` |
| `test/netns/` | Integration tests on network namespaces (build tag `netns`) |

## Developing

`make help` lists the targets. `make test` needs only the Go toolchain;
`make test-netns-docker` runs the namespace tests in a privileged container. CI runs
`lint`, `fmt-check`, `vet` and `test`.
