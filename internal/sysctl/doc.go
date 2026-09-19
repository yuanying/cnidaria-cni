// Package sysctl reads the kernel settings cnidaria depends on and refuses to start
// when they are not what the data plane needs: br_netfilter with
// bridge-nf-call-iptables and bridge-nf-call-ip6tables set (ADR 0002) and IP
// forwarding for both families.
package sysctl
