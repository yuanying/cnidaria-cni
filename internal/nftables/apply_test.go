package nftables

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner records what would have been fed to nft and fails on demand.
type fakeRunner struct {
	calls []fakeCall
	fail  error
}

type fakeCall struct {
	args  []string
	stdin string
}

func (f *fakeRunner) Run(_ context.Context, stdin string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{args: args, stdin: stdin})
	return nil, f.fail
}

const (
	textA = "table inet cnidaria\ndelete table inet cnidaria\ntable inet cnidaria {\n}\n"
	textB = "table inet cnidaria\ndelete table inet cnidaria\ntable inet cnidaria {\n\tchain x {\n\t}\n}\n"
)

func TestApplyFeedsTheTextToNFT(t *testing.T) {
	r := &fakeRunner{}
	a := NewApplier(r)
	changed, err := a.Apply(context.Background(), textA)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Errorf("first Apply reported no change")
	}
	if len(r.calls) != 1 {
		t.Fatalf("nft was run %d times, want 1", len(r.calls))
	}
	if got := strings.Join(r.calls[0].args, " "); got != "-f -" {
		t.Errorf("nft was run with %q, want \"-f -\"", got)
	}
	if r.calls[0].stdin != textA {
		t.Errorf("nft read %q, want the table text", r.calls[0].stdin)
	}
}

// The reconciler renders on every event; most renders produce the text already on the
// node, and those must not reach nft.
func TestApplySkipsTheTextAlreadyApplied(t *testing.T) {
	r := &fakeRunner{}
	a := NewApplier(r)
	ctx := context.Background()
	if _, err := a.Apply(ctx, textA); err != nil {
		t.Fatal(err)
	}
	changed, err := a.Apply(ctx, textA)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("reapplying the same text reported a change")
	}
	if len(r.calls) != 1 {
		t.Errorf("nft was run %d times, want 1", len(r.calls))
	}
	if _, err := a.Apply(ctx, textB); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Errorf("nft was run %d times after a different text, want 2", len(r.calls))
	}
}

// A failed nft -f changes nothing on the node (the file is one transaction), so the
// applier must not remember the text as applied, and the error must say what nft said.
func TestApplyFailureIsReportedAndNotRemembered(t *testing.T) {
	r := &fakeRunner{fail: errors.New("nft -f -: exit status 1: /dev/stdin:3:1-5: Error: syntax error")}
	a := NewApplier(r)
	ctx := context.Background()
	_, err := a.Apply(ctx, textA)
	if err == nil {
		t.Fatal("Apply returned no error")
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("error %q does not carry nft's message", err)
	}
	r.fail = nil
	changed, err := a.Apply(ctx, textA)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(r.calls) != 2 {
		t.Errorf("the text that failed was not retried (changed=%v, runs=%d)", changed, len(r.calls))
	}
}

// Forget is how the reconciler asks for the table to go back on even though nothing
// about it changed, which is what puts it back when it was removed behind the daemon's
// back.
func TestForgetMakesTheNextApplyRunNFT(t *testing.T) {
	r := &fakeRunner{}
	a := NewApplier(r)
	ctx := context.Background()
	for range 2 {
		if _, err := a.Apply(ctx, textA); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.calls) != 1 {
		t.Fatalf("nft was run %d times for one text, want 1", len(r.calls))
	}
	a.Forget()
	changed, err := a.Apply(ctx, textA)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(r.calls) != 2 {
		t.Errorf("the same text was not reapplied after Forget (changed=%v, runs=%d)", changed, len(r.calls))
	}
}
