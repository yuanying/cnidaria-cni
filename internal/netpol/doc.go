// Package netpol turns Kubernetes NetworkPolicy objects, together with the pods and
// namespaces they select, into the chain-and-set model that package nftables renders
// (ADR 0003). It implements the NetworkPolicy v1 semantics: podSelector,
// namespaceSelector, ipBlock with except, ports and endPort, policyTypes, and
// "selected by any policy means isolated". It reads no kernel state.
package netpol
