// Package routes keeps the host routing table pointing at the other nodes' pod
// CIDRs, one route per peer node and address family, with that node's InternalIP as
// the next hop (flannel host-gw style, ADR 0006). It owns only the routes carrying
// its protocol marker and never touches anyone else's.
package routes
