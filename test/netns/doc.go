// Package netns holds the integration tests that build "nodes" out of network
// namespaces, each with a bridge and pod namespaces, and check reachability and
// policy enforcement from the outside (ADR 0008). They need root / CAP_NET_ADMIN and
// the nft and ip commands, so they sit behind the build tag "netns" and
// "go test ./..." does not pick them up. Run them with "make test-netns" where the
// tools exist, or "make test-netns-docker" through the privileged container.
package netns
