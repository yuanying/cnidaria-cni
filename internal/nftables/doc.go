// Package nftables owns the one table cnidaria keeps on a node, "inet cnidaria"
// (ADR 0003). It renders the chain-and-set model into nft text and applies it with
// "nft -f" as a single atomic replacement of that table only. A permissive
// NodeNetworkPolicy differs from an enforcing one only in the final verdict it renders
// (ADR 0004). It never flushes the ruleset and never touches another table,
// including kube-proxy's.
package nftables
