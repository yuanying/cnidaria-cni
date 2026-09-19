//go:build netns

package netns

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// Asking for this suite is a statement that the tools are expected to be there
// (CNIDARIA_NETNS_REQUIRE=1, which the make target and the container image set).
// go test prints "ok" for a package whose only output is a skip reason, so a missing
// prerequisite fails loudly instead of skipping (ADR 0008).
func TestMain(m *testing.M) {
	for _, tool := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(tool); err == nil {
			continue
		}
		if os.Getenv("CNIDARIA_NETNS_REQUIRE") != "" {
			fmt.Fprintf(os.Stderr, "netns: %s is required but not on PATH\n", tool)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "netns: %s not on PATH, skipping the suite\n", tool)
		os.Exit(0)
	}
	if os.Geteuid() != 0 {
		if os.Getenv("CNIDARIA_NETNS_REQUIRE") != "" {
			fmt.Fprintln(os.Stderr, "netns: root is required")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "netns: not root, skipping the suite")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
