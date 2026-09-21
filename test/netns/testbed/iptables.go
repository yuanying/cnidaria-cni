package testbed

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/yuanying/cnidaria-cni/internal/iptables"
)

// nodeIPTables runs the iptables commands inside one node namespace, so that the
// forward chain is written to that "node" and to no other.
type nodeIPTables struct{ ns string }

func (r nodeIPTables) Run(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ip", append([]string{"netns", "exec", r.ns, name}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s in %s: %w: %s", name, strings.Join(args, " "), r.ns, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Forward returns the iptables forward chain of this node, with the backends chosen
// as the daemon on the node would choose them.
func (n *Node) Forward(t testing.TB) *iptables.Forward {
	t.Helper()
	f, err := iptables.Detect(context.Background(), nodeIPTables{ns: n.NS})
	if err != nil {
		t.Fatalf("iptables on node %s: %v", n.Name, err)
	}
	return f
}
