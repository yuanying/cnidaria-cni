//go:build netns

package nftables

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests need only one fresh network namespace and nft: they show that what the
// renderer produces is accepted, that a reapply changes nothing, and that other tables
// are left alone. Tests that need nodes and pods live in test/netns (ADR 0008).

// Asking for this suite is a statement that the tools are expected to be there
// (CNIDARIA_NETNS_REQUIRE=1, which the make target and the container image set), so a
// missing prerequisite fails loudly instead of skipping (ADR 0008).
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

// netnsRunner runs nft inside one network namespace, so that the test never touches
// the tables of the namespace the test process runs in.
type netnsRunner struct{ ns string }

func (r netnsRunner) Run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	return run(ctx, stdin, "ip", append([]string{"netns", "exec", r.ns, "nft"}, args...)...)
}

// newNetns creates a network namespace that is removed when the test ends.
func newNetns(t *testing.T) netnsRunner {
	t.Helper()
	ns := fmt.Sprintf("cnidaria-nft-%d-%d", os.Getpid(), time.Now().UnixNano())
	if out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput(); err != nil {
		t.Fatalf("ip netns add %s: %v: %s", ns, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("ip", "netns", "del", ns).CombinedOutput(); err != nil {
			t.Errorf("ip netns del %s: %v: %s", ns, err, out)
		}
	})
	return netnsRunner{ns: ns}
}

func (r netnsRunner) list(t *testing.T, family, table string) string {
	t.Helper()
	out, err := r.Run(context.Background(), "", "list", "table", family, table)
	if err != nil {
		t.Fatalf("nft list table %s %s: %v", family, table, err)
	}
	return string(out)
}

func render(t *testing.T, p Params) string {
	t.Helper()
	ruleset, err := Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return ruleset.String()
}

// What the golden files hold is what nft accepts.
func TestNFTAcceptsTheRenderedTable(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newNetns(t)
			if _, err := NewApplier(r).Apply(context.Background(), render(t, tc.params)); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			listed := r.list(t, TableFamily, TableName)
			for _, chain := range []string{ChainEgress, ChainIngress, ChainInput, ChainOutput, ChainPostrouting} {
				if !strings.Contains(listed, "chain "+chain+" {") {
					t.Errorf("the applied table has no chain %s:\n%s", chain, listed)
				}
			}
		})
	}
}

// The file adds, deletes and redeclares the table, so applying it onto a node that
// already holds it is the same operation and leaves the same table.
func TestReapplyLeavesTheTableAsItWas(t *testing.T) {
	r := newNetns(t)
	text := render(t, goldenCases[0].params)
	ctx := context.Background()
	if _, err := NewApplier(r).Apply(ctx, text); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	before := r.list(t, TableFamily, TableName)
	// A fresh applier has no memory of the text, so nft really runs again.
	if changed, err := NewApplier(r).Apply(ctx, text); err != nil || !changed {
		t.Fatalf("second Apply: changed=%v err=%v", changed, err)
	}
	if after := r.list(t, TableFamily, TableName); after != before {
		t.Errorf("the table changed on reapply.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// kube-proxy's tables sit next to ours (ADR 0003). A table someone else owns, with a
// chain on the same hook, has to survive an apply untouched.
func TestOtherTablesAreLeftAlone(t *testing.T) {
	r := newNetns(t)
	ctx := context.Background()
	other := "table ip filter {\n" +
		"\tchain INPUT {\n\t\ttype filter hook input priority filter; policy accept;\n" +
		"\t\ttcp dport 8080 counter accept\n\t}\n}\n"
	if _, err := r.Run(ctx, other, "-f", "-"); err != nil {
		t.Fatalf("installing the other table: %v", err)
	}
	before := r.list(t, "ip", "filter")
	if _, err := NewApplier(r).Apply(ctx, render(t, goldenCases[0].params)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if after := r.list(t, "ip", "filter"); after != before {
		t.Errorf("ip filter changed.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// A rejected file leaves nothing behind, and the error says what nft said.
func TestRejectedTextCarriesNFTsMessage(t *testing.T) {
	r := newNetns(t)
	ctx := context.Background()
	_, err := NewApplier(r).Apply(ctx, "table inet cnidaria {\n\tchain broken {\n\t\tthis is not a rule\n\t}\n}\n")
	if err == nil {
		t.Fatal("nft accepted a broken file")
	}
	if !strings.Contains(err.Error(), "Error:") {
		t.Errorf("error %q does not carry nft's message", err)
	}
	if _, err := r.Run(ctx, "", "list", "table", TableFamily, TableName); err == nil {
		t.Errorf("the table exists after a rejected apply")
	}
}
