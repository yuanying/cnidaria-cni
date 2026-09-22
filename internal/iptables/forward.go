// Package iptables keeps pod traffic from being dropped by an iptables FORWARD chain
// whose policy is DROP, which a container engine, a host firewall or a distribution's
// default may set. A verdict in cnidaria's own nftables table cannot override that
// drop, since a drop in any base chain at a hook is final (ADR 0003). So, like the CNIs
// it stands beside, cnidaria keeps a chain of its own in the filter table, CNIDARIA-FWD,
// that accepts traffic from and to the cluster's pod CIDRs, and one jump to it at the
// head of FORWARD. Those two are all it touches: not the policy, not any other rule or
// chain, and nothing is flushed but its own chain.
package iptables

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
)

// Chain is cnidaria's own chain in the filter table of each family.
const Chain = "CNIDARIA-FWD"

// jumpComment says where the jump in FORWARD came from to whoever lists the rules.
const jumpComment = "cnidaria: accept pod traffic ahead of the FORWARD policy"

// Runner runs an iptables command. It is an interface so that the commands are
// tested without iptables and the netns tests can run them inside a namespace.
type Runner interface {
	// Run runs name with args, feeding it stdin, and returns what it printed. A
	// non-zero exit is an error that carries what the command wrote to stderr and
	// wraps one with an ExitCode method, as *exec.ExitError has; a command that is
	// not installed is an error that wraps exec.ErrNotFound.
	Run(ctx context.Context, stdin, name string, args ...string) ([]byte, error)
}

// Exec runs the commands on this host.
type Exec struct{}

func (Exec) Run(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
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

// Backend is which of the two iptables implementations the rules go through. Rules
// written through one are invisible to the other, and a jump only works into a
// FORWARD that the kernel evaluates, so the choice has to match the node's.
type Backend string

const (
	NFT    Backend = "nft"
	Legacy Backend = "legacy"
)

// Forward keeps the chain and the jump for both families.
type Forward struct {
	run Runner
	// V4 and V6 are the backends chosen for iptables and ip6tables, and the
	// reasons say why, for the log.
	V4, V6             Backend
	V4Reason, V6Reason string
}

// Detect chooses the backend for each family, in this order:
//
//  1. The one holding more of kube-proxy's KUBE- chains, as kube-proxy and flannel
//     decide, since that is the one the node's FORWARD rules are in.
//  2. Failing that, the one the other family chose by rule 1: kube-proxy may run
//     single-stack, or not have written that family yet, and the node uses one
//     backend for both.
//  3. Failing that, nft, or legacy if only legacy is installed.
//
// A backend that cannot be read, legacy without its kernel module for one, counts as
// holding no KUBE- chains. It is an error only when neither is installed.
func Detect(ctx context.Context, r Runner) (*Forward, error) {
	v4, err := probe(ctx, r, "iptables")
	if err != nil {
		return nil, err
	}
	v6, err := probe(ctx, r, "ip6tables")
	if err != nil {
		return nil, err
	}
	f := &Forward{run: r}
	f.V4, f.V4Reason = choose(v4, v6)
	f.V6, f.V6Reason = choose(v6, v4)
	return f, nil
}

// family is what the two -save commands of one family showed.
type family struct {
	prog                string
	nftKube, legacyKube int
	nftInstalled        bool
}

func probe(ctx context.Context, r Runner, prog string) (family, error) {
	nftOut, nftErr := r.Run(ctx, "", prog+"-nft-save")
	legacyOut, legacyErr := r.Run(ctx, "", prog+"-legacy-save")
	nftMissing, legacyMissing := errors.Is(nftErr, exec.ErrNotFound), errors.Is(legacyErr, exec.ErrNotFound)
	if nftMissing && legacyMissing {
		return family{}, fmt.Errorf("neither %s-nft nor %s-legacy is installed: the image needs the iptables package", prog, prog)
	}
	return family{prog: prog, nftKube: kubeChains(nftOut), legacyKube: kubeChains(legacyOut), nftInstalled: !nftMissing}, nil
}

// byKubeProxy is rule 1: the backend with more KUBE- chains, if either has more.
func (f family) byKubeProxy() (Backend, bool) {
	switch {
	case f.legacyKube > f.nftKube:
		return Legacy, true
	case f.nftKube > f.legacyKube:
		return NFT, true
	}
	return "", false
}

func choose(f, other family) (Backend, string) {
	if b, ok := f.byKubeProxy(); ok {
		return b, "kube-proxy's KUBE- chains are in this backend"
	}
	if b, ok := other.byKubeProxy(); ok {
		return b, "no KUBE- chains here; following " + other.prog + ", where they are in this backend"
	}
	if !f.nftInstalled {
		return Legacy, "no KUBE- chains in either family, and only legacy is installed"
	}
	return NFT, "no KUBE- chains in either family; nft is the default"
}

// kubeChains counts the KUBE- chains declared in iptables-save output.
func kubeChains(save []byte) int {
	n := 0
	for l := range strings.Lines(string(save)) {
		if strings.HasPrefix(l, ":KUBE-") {
			n++
		}
	}
	return n
}

// Apply makes each family's chain accept traffic from and to that family's CIDRs, and
// puts the jump to it first in FORWARD if it is not in FORWARD already. A jump that is
// there but not first is left where it is. Both families are written even if one
// fails, and every failure is returned.
func (f *Forward) Apply(ctx context.Context, cidrs []netip.Prefix) error {
	var v4, v6 []netip.Prefix
	for _, p := range cidrs {
		if p.Addr().Is4() {
			v4 = append(v4, p.Masked())
		} else {
			v6 = append(v6, p.Masked())
		}
	}
	return errors.Join(
		f.apply(ctx, "iptables-"+string(f.V4), v4),
		f.apply(ctx, "ip6tables-"+string(f.V6), v6),
	)
}

func (f *Forward) apply(ctx context.Context, bin string, cidrs []netip.Prefix) error {
	// Declaring a chain to iptables-restore --noflush empties it, so this replaces
	// the chain's rules in one transaction: there is no moment in which the chain
	// is empty and pod traffic falls through to the policy.
	var in strings.Builder
	in.WriteString("*filter\n:" + Chain + " - [0:0]\n")
	slices.SortFunc(cidrs, netip.Prefix.Compare)
	for _, p := range slices.Compact(cidrs) {
		fmt.Fprintf(&in, "-A %s -s %s -j ACCEPT\n", Chain, p)
		fmt.Fprintf(&in, "-A %s -d %s -j ACCEPT\n", Chain, p)
	}
	in.WriteString("COMMIT\n")
	if _, err := f.run.Run(ctx, in.String(), bin+"-restore", "-w", "--noflush"); err != nil {
		return err
	}

	// -C exits 1 when the rule is not there. Any other failure says nothing about
	// the jump, and inserting one on it could leave two, so it is returned instead.
	jump := []string{"FORWARD", "-m", "comment", "--comment", jumpComment, "-j", Chain}
	_, err := f.run.Run(ctx, "", bin, append([]string{"-w", "-t", "filter", "-C"}, jump...)...)
	var exit interface{ ExitCode() int }
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &exit) || exit.ExitCode() != 1:
		return err
	}
	_, err = f.run.Run(ctx, "", bin, append([]string{"-w", "-t", "filter", "-I", "FORWARD", "1"}, jump[1:]...)...)
	return err
}
