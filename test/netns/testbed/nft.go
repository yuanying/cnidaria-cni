package testbed

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/yuanying/cnidaria-cni/internal/nftables"
)

// nodeNFT runs nft inside one node namespace, so that a table is applied to that
// "node" and to no other.
type nodeNFT struct{ ns string }

func (r nodeNFT) Run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ip", append([]string{"netns", "exec", r.ns, "nft"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nft %s in %s: %w: %s", strings.Join(args, " "), r.ns, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ApplyRuleset replaces the node's inet cnidaria table with text, which is what the
// daemon on that node does with what the renderers produced. The daemon itself is not
// in the loop here (ADR 0008).
func (n *Node) ApplyRuleset(t testing.TB, text string) {
	t.Helper()
	if _, err := nftables.NewApplier(nodeNFT{ns: n.NS}).Apply(context.Background(), text); err != nil {
		t.Fatalf("applying the ruleset on node %s: %v", n.Name, err)
	}
}
