package nftables

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs nft. It is an interface so that the applier is tested without nft and
// the netns tests can run nft inside a namespace.
type Runner interface {
	// Run runs nft with args, feeding it stdin, and returns what it printed. A
	// non-zero exit is an error that carries what nft wrote to stderr.
	Run(ctx context.Context, stdin string, args ...string) ([]byte, error)
}

// NFT runs the nft command on this host.
type NFT struct{}

func (NFT) Run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	return run(ctx, stdin, "nft", args...)
}

func run(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Applier replaces the table with "nft -f" and remembers what it applied, so that a
// render that produces the same text again does not reach nft.
type Applier struct {
	run     Runner
	applied string
}

func NewApplier(r Runner) *Applier {
	return &Applier{run: r}
}

// Apply replaces the table with text unless text is what was last applied. It reports
// whether nft was run. A failed nft -f leaves the node as it was, the file being one
// transaction, so the text is not remembered and the next Apply tries it again.
func (a *Applier) Apply(ctx context.Context, text string) (bool, error) {
	if text == a.applied {
		return false, nil
	}
	if _, err := a.run.Run(ctx, text, "-f", "-"); err != nil {
		return false, err
	}
	a.applied = text
	return true, nil
}
