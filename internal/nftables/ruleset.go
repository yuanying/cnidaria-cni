package nftables

import (
	"fmt"
	"strings"
)

// The one table cnidaria owns. Everything this package emits is inside it, and nothing
// outside it is read or written (ADR 0003).
const (
	TableFamily = "inet"
	TableName   = "cnidaria"
)

// Ruleset is the table: its sets, then its chains, in the order they are written.
//
// It is a structure rather than text so that the NetworkPolicy and NodePolicy renderers
// can add their sets and chains to what Render produced. String is the text to hand to
// "nft -f".
type Ruleset struct {
	Sets   []Set
	Chains []Chain
}

// Set is one named set inside the table.
type Set struct {
	Name     string
	Type     string   // ipv4_addr, ipv6_addr
	Flags    []string // interval, for a set that holds prefixes
	Elements []string // written as they appear in the output
	Comment  string   // written above the set as a file comment; empty leaves it out
}

// Chain is one chain. Base is nil for a chain that is only jumped to.
type Chain struct {
	Name    string
	Base    *BaseChain
	Rules   []Rule
	Comment string // written above the chain as a file comment; empty leaves it out
}

// BaseChain is the hook a chain hangs off.
type BaseChain struct {
	Type     string // filter, nat
	Hook     string // input, forward, output, postrouting
	Priority string // filter, "filter + 1", srcnat
	Policy   string // accept
}

// Rule is one rule. Every rule counts, so the text carries "counter" between the
// match and the verdict, and every rule says where it came from in an nftables
// comment, so that "nft list ruleset" reads in the operator's vocabulary (ADR 0003).
type Rule struct {
	Match   string // the match expressions; empty for a rule that matches everything
	Verdict string // accept, drop, return, masquerade, "jump <chain>"
	Comment string
}

// The header explains, to whoever reads the file on the node, why it looks like this.
const rulesetHeader = `# cnidaria's nftables table. Generated on every apply; edits here do not survive one.
#
# cnidaria owns this one table and nothing else in the ruleset, so the ruleset is never
# flushed and no rule anybody else installed, kube-proxy's included, is touched
# (ADR 0003). The two statements below add the table if it is not already there and
# then remove it, so that the one that follows goes in whether or not a previous apply
# left anything behind. nft runs the whole file as a single transaction, so the table
# is never half replaced.

`

// String is the text to hand to "nft -f": one transaction that adds the table, deletes
// it and declares it again.
func (r *Ruleset) String() string {
	var b strings.Builder
	b.WriteString(rulesetHeader)
	fmt.Fprintf(&b, "table %s %s\n", TableFamily, TableName)
	fmt.Fprintf(&b, "delete table %s %s\n\n", TableFamily, TableName)
	fmt.Fprintf(&b, "table %s %s {\n", TableFamily, TableName)

	blocks := make([]string, 0, len(r.Sets)+len(r.Chains))
	for _, set := range r.Sets {
		blocks = append(blocks, set.String())
	}
	for _, chain := range r.Chains {
		blocks = append(blocks, chain.String())
	}
	b.WriteString(strings.Join(blocks, "\n"))
	b.WriteString("}\n")
	return b.String()
}

func (s Set) String() string {
	var b strings.Builder
	if s.Comment != "" {
		fmt.Fprintf(&b, "\t# %s\n", s.Comment)
	}
	fmt.Fprintf(&b, "\tset %s {\n\t\ttype %s\n", s.Name, s.Type)
	if len(s.Flags) > 0 {
		fmt.Fprintf(&b, "\t\tflags %s\n", strings.Join(s.Flags, ","))
	}
	if len(s.Elements) > 0 {
		fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(s.Elements, ", "))
	}
	b.WriteString("\t}\n")
	return b.String()
}

func (c Chain) String() string {
	var b strings.Builder
	if c.Comment != "" {
		fmt.Fprintf(&b, "\t# %s\n", c.Comment)
	}
	fmt.Fprintf(&b, "\tchain %s {\n", c.Name)
	if c.Base != nil {
		fmt.Fprintf(&b, "\t\ttype %s hook %s priority %s; policy %s;\n",
			c.Base.Type, c.Base.Hook, c.Base.Priority, c.Base.Policy)
	}
	for _, rule := range c.Rules {
		fmt.Fprintf(&b, "\t\t%s\n", rule.String())
	}
	b.WriteString("\t}\n")
	return b.String()
}

func (r Rule) String() string {
	words := make([]string, 0, 4)
	if r.Match != "" {
		words = append(words, r.Match)
	}
	words = append(words, "counter", r.Verdict)
	if r.Comment != "" {
		words = append(words, fmt.Sprintf("comment %q", r.Comment))
	}
	return strings.Join(words, " ")
}
