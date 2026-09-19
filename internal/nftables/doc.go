// Package nftables owns the one table cnidaria keeps on a node, "inet cnidaria"
// (ADR 0003). It renders the chain-and-set model into nft text, applies it with
// "nft -f" as a single atomic replacement of that table only, and runs the
// commit-confirmed check that rolls a NodePolicy back when the API server stops
// being reachable after an apply (ADR 0004). It never flushes the ruleset and never
// touches another table, including kube-proxy's.
package nftables
