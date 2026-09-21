# cnidaria-cni

A Kubernetes CNI for small clusters whose nodes share one L2 segment: a bridge per
node, host routing between nodes (flannel `host-gw` style), IPv4 / IPv6 dual stack,
and — what flannel does not do — NetworkPolicy enforcement and a policy for the nodes
themselves, both in nftables.

The name is the phylum of jellyfish and anemones: it looks harmless until you touch
it.

日本語版は [README.ja.md](README.ja.md) を参照。

> **Status: alpha.** The API group is `v1alpha1` and the fields may still move. The
> data plane and the policy semantics are covered by tests that build nodes out of
> network namespaces and check reachability from inside pods, but no long-lived
> cluster has run on it yet.

## What it does

- Writes a CNI conflist per node that names the reference plugins `bridge`,
  `host-local` and `portmap`, with the node's `podCIDRs` as the address ranges. A pod
  gets an address from every family the node has a CIDR for — two on a dual-stack
  cluster, one on a single-stack one; hairpin and hostPort keep working.
- Keeps a route to every other node's pod CIDRs, per address family, with that node's
  InternalIP as the next hop.
- Renders NetworkPolicy v1 into one nftables table, `inet cnidaria`.
- Accepts a cluster-scoped `NodeNetworkPolicy` for the node's own `input` and `output`, with
  safe rules a policy cannot remove. A policy is permissive by default: what it would
  drop is logged and counted until `Enforce` is chosen.
- Source-NATs pod traffic that leaves the cluster's pod CIDRs.

## What it does not do

- **Replace kube-proxy.** Service load balancing stays where it is, in whichever mode
  kube-proxy runs; cnidaria's table sits beside kube-proxy's and touches nothing in it,
  and the ruleset is never flushed.
- **Overlay networking.** Nodes must reach each other directly on one segment. There
  is no VXLAN, no tunnel, no BGP.
- **Its own CNI binary or IPAM.** The reference plugins do the per-pod work; cnidaria
  is a node daemon and a conflist. Nothing of cnidaria's runs when a pod starts.
- **Fix the node for you.** A kernel setting the data plane depends on is a refusal to
  start, not something the daemon turns on.

## Requirements

| Requirement | Why |
|---|---|
| `br_netfilter` loaded, with `net.bridge.bridge-nf-call-iptables` and `bridge-nf-call-ip6tables` at 1 | Pod traffic that stays on one node is switched by the bridge and reaches the IP filter hooks only through this. The daemon checks it at start-up and refuses to run without it (ADR 0002) |
| `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding` at 1 | The node routes between its bridge and the segment. Checked at start-up like the settings above, with the same refusal to run |
| kube-controller-manager with `--allocate-node-cidrs` and the cluster CIDRs | `node.spec.podCIDRs` is the only source of a node's ranges. For dual stack, one CIDR per family |
| Every node with an InternalIP of each family in use | A route's next hop is the peer's InternalIP of the same family. A missing family means no routes for that family, logged as a warning (ADR 0006) |
| All nodes on one L2 segment | Next hops have to be on-link |
| kube-proxy, in either mode | Services are its job, not cnidaria's. Nothing here depends on kube-proxy's tables or chains, only on DNAT happening before the forward hook, which both the iptables and the nftables mode do (ADR 0003) |

The nodes need nothing installed: the image carries the daemon, `nft` and the
reference plugins.

## Install

```sh
kubectl apply -k deploy
```

That installs five objects: the `NodeNetworkPolicy` CRD, a ClusterRole and a
ClusterRoleBinding, all three cluster-scoped, and a ServiceAccount and the DaemonSet in
`kube-system`. The image tag to deploy is the `images:` entry of
`deploy/kustomization.yaml`, which an overlay can override; images are published to
`ghcr.io/yuanying/cnidaria-cni`.

The DaemonSet runs with `hostNetwork`, `CAP_NET_ADMIN` and nothing else — not
privileged — and its init container copies `bridge`, `host-local` and `portmap` into
`/opt/cni/bin` without disturbing any other plugin the node has.

A node is working when its conflist is at `/etc/cni/net.d/10-cnidaria.conflist`,
`ip route show proto 200` lists the other nodes' pod CIDRs, and the table
`inet cnidaria` is there.

## NodeNetworkPolicy: the node's own traffic

NetworkPolicy governs pods only. `NodeNetworkPolicy` is cluster-scoped, selects nodes by
label, and is rendered into the node's `input` and `output` chains (ADR 0004).

Two things keep it from locking a node out.

**Safe rules.** Every `input` and `output` chain opens with rules no policy can
remove: established and related traffic, loopback, and the ICMP and ICMPv6 types a
host needs for path MTU discovery and neighbour discovery. On input they add SSH,
kubelet, the API server, etcd and the NodePort range; on output, the API server, etcd,
kubelet and DNS, and this node's own pod CIDRs, which is what keeps kubelet probing and
`exec`ing into the pods it hosts. Their ports are the conventional ones; the full list
is in [ADR 0004](docs/adr/0004-node-policy-crd-and-lockout-prevention.md).

**Permissive by default.** A policy without `spec.mode` is rendered in full but drops
nothing: what `Enforce` would drop is logged and counted instead. The way to put a
policy into service is therefore:

1. Apply it as it is. A `NodeNetworkPolicy` with no `mode` is `Permissive`.
2. Watch what it would have dropped, for as long as the node's traffic pattern needs —
   a full day for a node whose backups run at night.

   ```sh
   journalctl -k | grep cnidaria-nodenetworkpolicy   # source, destination, protocol, port
   nft list chain inet cnidaria input         # the counters, which are not rate-limited
   kubectl get nodenetworkpolicies                   # the mode each policy asks for
   kubectl get nodenetworkpolicy NAME -o yaml        # status.nodes[]: what each node got
   ```

   The column `kubectl get` prints is `spec.mode`, which is what the policy asks for.
   What a node arrived at is its entry in `status.nodes[]`, and the two differ while
   another permissive policy shares a direction with this one.

3. Add the entries the log shows are missing, then set `spec.mode: Enforce`.

Nothing about the ruleset changes between the two modes except the final verdict, so
what was observed in `Permissive` is exactly what `Enforce` does.

A policy names its peers by address, never by pod or namespace selector: a node is
addressed by the network, not by the cluster. Pods are reached through their CIDRs
like anything else.

```yaml
apiVersion: cnidaria.unstable.cloud/v1alpha1
kind: NodeNetworkPolicy
metadata:
  name: workers
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  ingress:
    - from:
        - ipBlock:
            cidr: 192.0.2.0/24
            except: [192.0.2.128/25]
      ports:
        - port: 9100          # protocol defaults to TCP, as in NetworkPolicy
```

A node selected by no policy of a direction is open in that direction; a node selected
by one accepts only what its rules list. Policies selecting one node are unioned, and
a node enforces a direction only when **every** policy closing that direction on it
asks for `Enforce` — one permissive policy keeps the node observing.

## NetworkPolicy: what is enforced

NetworkPolicy v1 is rendered whole: `podSelector`, `namespaceSelector`, `ipBlock` with
`except`, ports by number and by name, `endPort` ranges, TCP / UDP / SCTP,
`policyTypes`, and "a pod selected by any policy of a direction is isolated in that
direction". IPv4 and IPv6 are handled symmetrically. A node enforces the policies of
the pods it hosts, so a packet meets the source pod's egress rules on the node it
leaves and the destination pod's ingress rules on the node it arrives at.

What is outside it:

| Not covered | Why |
|---|---|
| Pods with `hostNetwork` | They carry the node's addresses. A policy that named one would name the node; `NodeNetworkPolicy` is what governs that traffic |
| A node reaching a pod on itself | It leaves the node's own `output`, not the forward path, so a pod ingress policy never sees it. Kubelet probes and `exec` depend on this, and one of the safe rules above keeps it open |
| Terminated pods | A pod that has Succeeded or Failed keeps its addresses in the API until something deletes it, and one of them may already belong to a running pod. They are left out of every selection |
| Observing mode for NetworkPolicy | `Permissive` exists for `NodeNetworkPolicy` only. A pod policy drops from the moment it is applied, as it does everywhere else |

## Configuration

The daemon takes its node's name and otherwise defaults to what a node needs.

| Flag | Default | What it is for |
|---|---|---|
| `--node-name` | `$NODE_NAME` | The node this copy looks after. The DaemonSet fills it from the downward API |
| `--conflist` | `/etc/cni/net.d/10-cnidaria.conflist` | Where the conflist is written |
| `--mtu` | `0` | MTU for the bridge and the pod interfaces. 0 reads it from the interface holding the node's InternalIP |
| `--ipam-store-name` | the network name | The directory `host-local` keeps its leases in, under `/var/lib/cni/networks`. Set it to another CNI's network name to take over the addresses that CNI has already handed out (ADR 0009) |
| `--health-addr` | `127.0.0.1:19080` | `/healthz` and `/readyz`. Loopback only, which is why the DaemonSet's probes name `127.0.0.1` |
| `--metrics-addr` | `0` | Prometheus metrics, off by default |

The flags controller-runtime and its logger register (`--kubeconfig`, `--zap-*` and the
rest) are left out of the table.

## Layout

| Path | What is there |
|---|---|
| `cmd/cnidaria` | The node daemon |
| `internal/` | One package per responsibility; each package comment says what it owns |
| `deploy/` | kustomize manifests: CRD, RBAC, DaemonSet |
| `docs/adr/` | Design decisions, with Japanese under `docs/ja/adr/` |
| `test/netns/` | Integration tests on network namespaces (build tag `netns`) |

## Developing

`make help` lists the targets. `make test` needs only the Go toolchain;
`make test-netns-docker` builds nodes out of network namespaces and runs the
integration tests against them in a privileged container. `make image` builds the
`linux/amd64` and `linux/arm64` image with `docker buildx`. CI runs `lint`,
`fmt-check`, `vet` and `test`.

Read [docs/adr](docs/adr/README.md) before changing anything: the decisions there are
what the code is shaped around, and overturning one starts with a new record.
