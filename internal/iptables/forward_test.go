package iptables

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fakeRunner answers for iptables without running it. It records every command in
// the order it was issued, and replies from a table keyed on the command line.
type fakeRunner struct {
	log     strings.Builder
	replies map[string]reply
}

type reply struct {
	out string
	err error
}

func (f *fakeRunner) Run(_ context.Context, stdin, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	fmt.Fprintf(&f.log, "$ %s\n", line)
	for l := range strings.Lines(stdin) {
		fmt.Fprintf(&f.log, "  %s", l)
	}
	r := f.replies[line]
	return []byte(r.out), r.err
}

// exitError is a command that ran and exited non-zero, as *exec.ExitError is.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// A missing rule is what iptables -C reports with exit status 1.
var errExit1 = exitError(1)

const checkJump = "-w -t filter -C FORWARD -m comment --comment cnidaria: accept pod traffic ahead of the FORWARD policy -j CNIDARIA-FWD"

func prefixes(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("commands differ from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// The commands a reconcile issues are fixed by golden files, since what they may
// touch on the node is the whole point: one chain of our own, replaced in one
// transaction, and one jump at the head of FORWARD (ADR 0003).
func TestApplyIssues(t *testing.T) {
	cidrs := prefixes("192.0.2.0/24", "2001:db8:1::/64", "198.51.100.0/24", "2001:db8:2::/64")
	for _, tc := range []struct {
		name    string
		cidrs   []netip.Prefix
		replies map[string]reply
		v6      Backend
	}{
		// Nothing of ours on the node yet: the chains are written and the
		// jumps go in first in FORWARD.
		{name: "first", cidrs: cidrs, replies: map[string]reply{
			"iptables-nft " + checkJump:  {err: errExit1},
			"ip6tables-nft " + checkJump: {err: errExit1},
		}},
		// The jumps are there already and are left alone; the chains are
		// written all the same, which is what puts back a rule removed behind
		// the daemon's back.
		{name: "existing", cidrs: cidrs},
		// A node joined: its CIDRs are added to the chains.
		{name: "changed", cidrs: append(prefixes("203.0.113.0/24", "2001:db8:3::/64"), cidrs...)},
		// Each family goes through the backend chosen for it.
		{name: "legacy-v6", cidrs: cidrs, v6: Legacy, replies: map[string]reply{
			"ip6tables-legacy " + checkJump: {err: errExit1},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{replies: tc.replies}
			f := &Forward{run: r, V4: NFT, V6: NFT}
			if tc.v6 != "" {
				f.V6 = tc.v6
			}
			if err := f.Apply(t.Context(), tc.cidrs); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			golden(t, tc.name, r.log.String())
		})
	}
}

// A failure in one family is returned, and the other family is still written.
func TestApplyReturnsFailuresOfEitherFamily(t *testing.T) {
	r := &fakeRunner{replies: map[string]reply{
		"iptables-nft-restore -w --noflush": {err: errors.New("iptables-nft-restore: exit status 4: Another app is currently holding the xtables lock")},
	}}
	f := &Forward{run: r, V4: NFT, V6: NFT}
	err := f.Apply(t.Context(), prefixes("192.0.2.0/24", "2001:db8:1::/64"))
	if err == nil || !strings.Contains(err.Error(), "xtables lock") {
		t.Errorf("Apply returned %v, want the IPv4 failure", err)
	}
	if !strings.Contains(r.log.String(), "$ ip6tables-nft-restore") {
		t.Errorf("IPv6 was not written after IPv4 failed:\n%s", r.log.String())
	}
}

// Only exit status 1 from -C means the jump is missing. Anything else, such as the
// lock being held or a resource problem, says nothing about the jump, and inserting
// one then could leave two; it is returned for the queue to retry instead.
func TestApplyReturnsACheckThatFailedForAnotherReason(t *testing.T) {
	r := &fakeRunner{replies: map[string]reply{
		"iptables-nft " + checkJump: {err: exitError(4)},
	}}
	f := &Forward{run: r, V4: NFT, V6: NFT}
	err := f.Apply(t.Context(), prefixes("192.0.2.0/24"))
	if err == nil || !strings.Contains(err.Error(), "exit status 4") {
		t.Errorf("Apply returned %v, want the failed check", err)
	}
	if strings.Contains(r.log.String(), "iptables-nft -w -t filter -I") {
		t.Errorf("a jump was inserted after a check that did not say it was missing:\n%s", r.log.String())
	}
}

const kubeProxySave = `*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:KUBE-FORWARD - [0:0]
:KUBE-SERVICES - [0:0]
-A FORWARD -m comment --comment "kubernetes forwarding rules" -j KUBE-FORWARD
COMMIT
`

const dockerOnlySave = `*filter
:FORWARD DROP [0:0]
:DOCKER - [0:0]
COMMIT
`

// The backend is the one kube-proxy writes through, told apart by its KUBE- chains.
// A family without them follows the family that has them, and with none anywhere
// the backend is nft, or legacy where only legacy is installed.
func TestDetectChoosesTheBackendWithKubeChains(t *testing.T) {
	notFound := &exec.Error{Name: "x", Err: exec.ErrNotFound}
	for _, tc := range []struct {
		name    string
		replies map[string]reply
		v4, v6  Backend
	}{
		{name: "kube-proxy on nft", v4: NFT, v6: NFT, replies: map[string]reply{
			"iptables-nft-save":     {out: kubeProxySave},
			"iptables-legacy-save":  {out: dockerOnlySave},
			"ip6tables-nft-save":    {out: kubeProxySave},
			"ip6tables-legacy-save": {},
		}},
		{name: "kube-proxy on legacy", v4: Legacy, v6: Legacy, replies: map[string]reply{
			"iptables-nft-save":     {out: dockerOnlySave},
			"iptables-legacy-save":  {out: kubeProxySave},
			"ip6tables-nft-save":    {},
			"ip6tables-legacy-save": {out: kubeProxySave},
		}},
		{name: "families apart", v4: Legacy, v6: NFT, replies: map[string]reply{
			"iptables-legacy-save": {out: kubeProxySave},
			"ip6tables-nft-save":   {out: kubeProxySave},
		}},
		// kube-proxy runs single-stack, or has not written one family yet: the
		// family without KUBE- chains follows the one that has them.
		{name: "v6 follows v4", v4: Legacy, v6: Legacy, replies: map[string]reply{
			"iptables-legacy-save": {out: kubeProxySave},
		}},
		{name: "v4 follows v6", v4: Legacy, v6: Legacy, replies: map[string]reply{
			"ip6tables-legacy-save": {out: kubeProxySave},
		}},
		// With no KUBE- chains anywhere, what is installed decides.
		{name: "only legacy installed", v4: Legacy, v6: Legacy, replies: map[string]reply{
			"iptables-nft-save":  {err: notFound},
			"ip6tables-nft-save": {err: notFound},
		}},
		{name: "no kube-proxy", v4: NFT, v6: NFT, replies: map[string]reply{
			"iptables-legacy-save": {out: dockerOnlySave},
		}},
		// legacy cannot even be read without its kernel module: that is no
		// KUBE- chains there, not a failure.
		{name: "legacy unreadable", v4: NFT, v6: NFT, replies: map[string]reply{
			"iptables-legacy-save":  {err: errors.New("exit status 1: Cannot initialize: Permission denied")},
			"ip6tables-legacy-save": {err: notFound},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Detect(t.Context(), &fakeRunner{replies: tc.replies})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if f.V4 != tc.v4 || f.V6 != tc.v6 {
				t.Errorf("chose v4=%s v6=%s, want v4=%s v6=%s", f.V4, f.V6, tc.v4, tc.v6)
			}
			if f.V4Reason == "" || f.V6Reason == "" {
				t.Errorf("no reason given for the choice: v4=%q v6=%q", f.V4Reason, f.V6Reason)
			}
		})
	}
}

// Without the binaries there is no way to keep pod traffic past a DROP policy, and
// the daemon refuses to start saying which package is missing.
func TestDetectWithoutIPTablesNamesThePackage(t *testing.T) {
	notFound := &exec.Error{Name: "x", Err: exec.ErrNotFound}
	r := &fakeRunner{replies: map[string]reply{}}
	for _, name := range []string{"iptables-nft-save", "iptables-legacy-save", "ip6tables-nft-save", "ip6tables-legacy-save"} {
		r.replies[name] = reply{err: notFound}
	}
	_, err := Detect(t.Context(), r)
	if err == nil || !strings.Contains(err.Error(), "iptables package") {
		t.Errorf("Detect returned %v, want an error naming the iptables package", err)
	}
}
