// Command cnidaria is the node daemon. One copy runs on every node (a DaemonSet with
// hostNetwork) and looks after that node only: it writes the CNI conflist, keeps the
// host-gw routes to the other nodes, and renders NetworkPolicy and NodePolicy into
// the node's nftables table (ADR 0001, 0003, 0007).
//
// This is the entry point only. Wiring the controller-runtime manager, the
// reconcilers and the start-up checks is the job of the units that implement them.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cnidaria: not implemented yet")
	os.Exit(2)
}
