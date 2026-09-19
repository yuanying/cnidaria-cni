// Package nodepol turns NodePolicy objects (ADR 0004) into the input and output part
// of the chain-and-set model that package nftables renders. The safe rules that keep a
// node reachable (established traffic, loopback, the ICMP types a host needs, SSH,
// kubelet, the API server, etcd, the NodePort range) are emitted here ahead of
// anything a policy declares and cannot be removed by a policy.
package nodepol
