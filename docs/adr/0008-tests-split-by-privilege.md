# ADR 0008: Tests are split by the privilege they need, and the privileged ones run in a container

- Status: Accepted (2026-09-19)

## Context

Most of what cnidaria does can be checked without a kernel: rendering a conflist,
turning NetworkPolicy objects into a chain model, rendering that model into nft text,
choosing routes. Some of it cannot: whether a packet actually gets from one pod to
another through a bridge, a route and a ruleset, and whether a policy actually drops
what it says it drops. Those need root, `CAP_NET_ADMIN`, network namespaces and the
`nft` and `ip` commands.

The same tests must run on a machine that has no `nft` and on which nothing is to be
installed, so the tooling has to travel with the tests. regied faced the same
constraints and settled them in its
[ADR 0010](https://github.com/yuanying/regied/blob/main/docs/adr/0010-netns-testbed.md);
this record adopts that answer and notes where cnidaria's testbed differs.

## Decision

### Two layers, separated by a build tag

| Layer | Command | Needs | Contains |
|---|---|---|---|
| unit | `make test` | the Go toolchain | Pure functions with fixed inputs: renderers (golden files), semantics (tables of cases), reconcile logic against fake objects |
| netns | `make test-netns`, or `make test-netns-docker` | root, `nft`, `ip` | "Nodes" built from network namespaces, exercised from the outside |

The netns tests are behind the build tag `netns` so `go test ./...` never picks them
up. `go vet` and the linter run with the tag as well so that the tagged files do not
rot.

### The netns suite fails loudly when it cannot run

Asking for the suite is a statement that the tools are expected. The make target and
the container image set `CNIDARIA_NETNS_REQUIRE=1`, under which a missing tool or a
non-root process is a failure with the reason, not a skip. `go test` prints `ok` for a
package whose only output was a skip message, and a run that never happened must not
read as green. A bare `go test -tags netns` without the variable still skips, for
someone poking at it locally.

### Run in a privileged sibling container

`make test-netns-docker` builds an image with `nft`, `ip` and the Go toolchain, starts
it `--privileged` with the repository bind-mounted at the same path, and runs
`make test-netns` inside. Nothing is installed on the host. The Go build and module
caches are mounted from the invoking user's cache directory so that a run does not
rebuild the standard library.

### What the testbed is

Each "node" is a network namespace holding a bridge, a veth pair to a shared "segment"
namespace that stands in for the L2 network, and the same sysctls a real node has
(ADR 0002). Pods are further namespaces attached to the bridge through the real
reference plugins driven by the real conflist (ADR 0001), so the tests exercise the
plugins and not a stand-in. Addresses are from the documentation ranges (RFC 5737,
RFC 3849).

Assertions are made from pod and node namespaces, never by reading the device under
test's tables: a test that inspects the ruleset only holds for one rendering, while a
test that sends a packet holds for any. Where the difference matters, "dropped" and
"nothing listening" are told apart by timing, as regied's testbed does.

The daemon is not in the loop for the first units: the tests apply routes and rulesets
that the unit-tested renderers produced. Putting the daemon itself in a namespace
against a fake API server is a later addition and does not change the layout.

## Consequences

- The scaffold ships the harness with zero tests, and `make test-netns-docker` proves
  the harness end to end by reporting no tests to run. Each implementation unit adds its
  own.
- CI runs the unit layer only. The netns layer runs on a developer machine or a runner
  that allows privileged containers, and the task's completion criteria name it
  explicitly.
- Kernel differences between the development host and the nodes (an arm64 vendor
  kernel) are not covered here; the netns layer is a gate before the cluster, not a
  substitute for it.
