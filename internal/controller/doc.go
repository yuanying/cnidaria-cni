// Package controller holds the controller-runtime reconcilers (ADR 0007): the route
// reconciler driven by Node events, the conflist writer for this node, and the
// ruleset reconciler that every Pod, Namespace, NetworkPolicy and NodePolicy event
// funnels into one recompute of the node's nftables table.
package controller
