package nodepol

import (
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/yuanying/cnidaria-cni/internal/apis/v1alpha1"
	"github.com/yuanying/cnidaria-cni/internal/nftables"
)

// The chains this package adds. A dispatch chain holds one jump per policy that
// closes its direction, and ends in the verdict for a packet none of them accepted.
// The base chains keep their safe rules and gain one jump behind them (ADR 0004).
const (
	ChainInputDispatch  = "input_dispatch"
	ChainOutputDispatch = "output_dispatch"
)

// What permissive mode writes to the kernel log. The prefix is part of the interface:
// tools reading the journal look for it (ADR 0004).
const (
	logPrefix = "cnidaria-nodepolicy "
	logRate   = "10/second"
)

// Node is the node the table is rendered for. Only the labels matter here: they decide
// which policies select it. The name is carried for the messages.
type Node struct {
	Name   string
	Labels map[string]string
}

// PolicyError is a policy that could not be rendered, and says which one. The daemon
// puts the message in that policy's status rather than in every policy's (ADR 0004).
type PolicyError struct {
	Policy string
	Err    error
}

func (e *PolicyError) Error() string { return fmt.Sprintf("nodepol: policy %s: %s", e.Policy, e.Err) }

func (e *PolicyError) Unwrap() error { return e.Err }

func policyError(policy string, err error) error { return &PolicyError{Policy: policy, Err: err} }

// Add renders every NodePolicy that selects the node into rs and reports the mode each
// of those policies took effect in, which is what the node writes back to the status of
// each (ADR 0004). A policy asking for Enforce reports Permissive while a permissive
// policy shares a direction with it, because in that direction nothing is dropped.
//
// A direction no policy closes is left exactly as Render produced it: no dispatch
// chain, no jump, and so no verdict — the node stays open that way.
//
// An error means nothing was added: the caller has the table it passed in, and a
// message to put in the status of the policy the error names.
//
// One call renders one node's whole node policy. Calling it twice on the same table
// declares the dispatch chains twice and jumps into them twice, which nft refuses, so
// a reconcile renders a fresh table rather than adding to the last one (ADR 0003).
func Add(rs *nftables.Ruleset, node Node, policies []v1alpha1.NodePolicy) (map[string]v1alpha1.Mode, error) {
	selected, err := selects(node, policies)
	if err != nil {
		return nil, err
	}
	// Rendering happens into a copy so that a policy rejected halfway through leaves
	// the caller's table untouched.
	next := &nftables.Ruleset{Sets: rs.Sets, Chains: slices.Clone(rs.Chains)}
	modes := map[string]v1alpha1.Mode{}
	for _, d := range []direction{ingress, egress} {
		closing := d.closedBy(selected)
		if len(closing) == 0 {
			continue
		}
		mode := effectiveMode(closing)
		jumps := make([]nftables.Rule, 0, len(closing))
		for _, p := range closing {
			chain, err := d.chainFor(p)
			if err != nil {
				return nil, err
			}
			next.Chains = append(next.Chains, chain)
			jumps = append(jumps, nftables.Rule{
				Verdict: "jump " + chain.Name,
				Comment: comment(p.Name),
			})
			// A policy that closes both directions is enforcing only where both
			// of them are.
			if modes[p.Name] != v1alpha1.ModePermissive {
				modes[p.Name] = mode
			}
		}
		next.Chains = append(next.Chains, nftables.Chain{
			Name:    d.dispatch,
			Comment: fmt.Sprintf("NodePolicy %s: one jump per policy, then what %s does with the rest", d.name, mode),
			Rules:   append(jumps, verdict(mode)...),
		})
		if err := jumpFrom(next, d.chain, d.dispatch); err != nil {
			return nil, err
		}
	}
	*rs = *next
	return modes, nil
}

// direction is one of the two things a NodePolicy governs, and everything that differs
// between them.
type direction struct {
	name       string // as policyTypes spells it, for the messages
	policyType v1alpha1.PolicyType
	chain      string // the base chain the safe rules are in
	dispatch   string
	prefix     string // of the chain name rendered for one policy
	addr       string // the end of the traffic the peers describe
}

var (
	ingress = direction{
		name:       "ingress",
		policyType: v1alpha1.PolicyTypeIngress,
		chain:      nftables.ChainInput,
		dispatch:   ChainInputDispatch,
		prefix:     "nodepol_in_",
		addr:       "saddr",
	}
	egress = direction{
		name:       "egress",
		policyType: v1alpha1.PolicyTypeEgress,
		chain:      nftables.ChainOutput,
		dispatch:   ChainOutputDispatch,
		prefix:     "nodepol_out_",
		addr:       "daddr",
	}
)

// entry is one ingress or egress rule with the direction's vocabulary taken out.
type entry struct {
	peers []v1alpha1.NodePolicyPeer
	ports []v1alpha1.NodePolicyPort
}

// closedBy keeps the policies that close this direction. Absent policyTypes mean what
// they mean in NetworkPolicy: ingress always, egress when egress rules are there.
func (d direction) closedBy(policies []v1alpha1.NodePolicy) []v1alpha1.NodePolicy {
	var closing []v1alpha1.NodePolicy
	for _, p := range policies {
		closes := d.policyType == v1alpha1.PolicyTypeIngress || len(p.Spec.Egress) > 0
		if len(p.Spec.PolicyTypes) > 0 {
			closes = slices.Contains(p.Spec.PolicyTypes, d.policyType)
		}
		if closes {
			closing = append(closing, p)
		}
	}
	return closing
}

func (d direction) entries(p v1alpha1.NodePolicy) []entry {
	var entries []entry
	if d.policyType == v1alpha1.PolicyTypeIngress {
		for _, r := range p.Spec.Ingress {
			entries = append(entries, entry{peers: r.From, ports: r.Ports})
		}
		return entries
	}
	for _, r := range p.Spec.Egress {
		entries = append(entries, entry{peers: r.To, ports: r.Ports})
	}
	return entries
}

// chainFor renders one policy's rules for this direction: every peer with every port,
// one rule each, all of them accepting. A policy with no rules for the direction gets
// an empty chain, which accepts nothing and is what "deny everything else" looks like.
func (d direction) chainFor(p v1alpha1.NodePolicy) (nftables.Chain, error) {
	name, err := nftables.Identifier(d.prefix, p.Name)
	if err != nil {
		return nftables.Chain{}, policyError(p.Name, err)
	}
	var rules []nftables.Rule
	for _, e := range d.entries(p) {
		matches, err := d.matches(e)
		if err != nil {
			return nftables.Chain{}, policyError(p.Name, fmt.Errorf("%s: %w", d.name, err))
		}
		for _, m := range matches {
			rules = append(rules, nftables.Rule{Match: m, Verdict: "accept", Comment: comment(p.Name)})
		}
	}
	return nftables.Chain{
		Name:    name,
		Comment: fmt.Sprintf("NodePolicy %s, %s", p.Name, d.name),
		Rules:   rules,
	}, nil
}

// effectiveMode is Enforce only when every policy closing the direction asks for it:
// one permissive policy keeps the node observing (ADR 0004). An empty mode is the
// Permissive the CRD defaults to.
func effectiveMode(policies []v1alpha1.NodePolicy) v1alpha1.Mode {
	for _, p := range policies {
		if p.Spec.Mode != v1alpha1.ModeEnforce {
			return v1alpha1.ModePermissive
		}
	}
	return v1alpha1.ModeEnforce
}

// verdict ends the dispatch chain. Enforce drops. Permissive logs and counts and lets
// the packet fall out of the chain, where the base chain's policy accept takes it.
//
// The log and the count are two rules because the rate limit must not reach the count:
// a busy node samples its log, and the counter is what says how much was sampled away
// (ADR 0004).
func verdict(mode v1alpha1.Mode) []nftables.Rule {
	if mode == v1alpha1.ModeEnforce {
		return []nftables.Rule{{Verdict: "drop", Comment: "nodepolicy: no rule accepted"}}
	}
	return []nftables.Rule{
		{
			Match:   "limit rate " + logRate,
			Verdict: fmt.Sprintf("log prefix %q", logPrefix),
			Comment: "nodepolicy: permissive, logged",
		},
		{Verdict: "continue", Comment: "nodepolicy: permissive, counted and accepted"},
	}
}

// selects keeps the policies whose nodeSelector matches the node, in name order so
// that the table does not depend on the order the policies were listed in.
func selects(node Node, policies []v1alpha1.NodePolicy) ([]v1alpha1.NodePolicy, error) {
	var selected []v1alpha1.NodePolicy
	for _, p := range policies {
		selector, err := metav1.LabelSelectorAsSelector(&p.Spec.NodeSelector)
		if err != nil {
			return nil, policyError(p.Name, fmt.Errorf("nodeSelector: %w", err))
		}
		if selector.Matches(labels.Set(node.Labels)) {
			selected = append(selected, p)
		}
	}
	slices.SortFunc(selected, func(a, b v1alpha1.NodePolicy) int {
		return strings.Compare(a.Name, b.Name)
	})
	return selected, nil
}

// jumpFrom sends the base chain into its dispatch chain, behind the safe rules that
// are already there.
func jumpFrom(rs *nftables.Ruleset, chain, dispatch string) error {
	for i := range rs.Chains {
		if rs.Chains[i].Name != chain {
			continue
		}
		rules := slices.Clone(rs.Chains[i].Rules)
		rs.Chains[i].Rules = append(rules, nftables.Rule{
			Verdict: "jump " + dispatch,
			Comment: "nodepolicy: everything the safe rules did not accept",
		})
		return nil
	}
	return fmt.Errorf("nodepol: the table has no chain %s to put the NodePolicy jump in", chain)
}
