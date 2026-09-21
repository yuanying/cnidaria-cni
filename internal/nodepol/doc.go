// Package nodepol turns NodeNetworkPolicy objects (ADR 0004) into the chains that decide what
// reaches and what leaves the node itself. It adds them to the table package nftables
// rendered, behind the safe rules that package already put in the input and output
// chains, and ends each direction in the verdict the policies' mode asks for: a drop
// under Enforce, a log and a count under Permissive.
//
// It knows nothing about Kubernetes clients: it takes the node's labels and the
// policies as values and returns what to apply, so it is tested without a cluster.
package nodepol
