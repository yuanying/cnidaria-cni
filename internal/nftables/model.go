package nftables

// The NetworkPolicy and NodeNetworkPolicy renderers add to the table Render produced: they
// append their own sets and chains, fill the sets Render left empty, and put rules
// into the chains it left for them. These are the accessors that needs. Nothing here
// changes what Render itself emits.

// The pointer Set and Chain return is into the table's own slice. It is good until
// something appends to r.Sets or r.Chains, which may move the backing array; a
// renderer that adds sets or chains has to look up again afterwards rather than hold
// one across the addition.

// Set returns the set of that name, or nil if the table has none.
func (r *Ruleset) Set(name string) *Set {
	for i := range r.Sets {
		if r.Sets[i].Name == name {
			return &r.Sets[i]
		}
	}
	return nil
}

// Chain returns the chain of that name, or nil if the table has none.
func (r *Ruleset) Chain(name string) *Chain {
	for i := range r.Chains {
		if r.Chains[i].Name == name {
			return &r.Chains[i]
		}
	}
	return nil
}

// InsertBeforeLast puts rules ahead of the chain's last rule. A dispatch chain ends in
// the drop that denies an isolated pod no policy accepted (ADR 0003), and the jumps
// into the policy chains have to go in front of it. On an empty chain it appends.
func (c *Chain) InsertBeforeLast(rules ...Rule) {
	if len(rules) == 0 {
		return
	}
	if len(c.Rules) == 0 {
		c.Rules = rules
		return
	}
	head, last := c.Rules[:len(c.Rules)-1], c.Rules[len(c.Rules)-1]
	kept := make([]Rule, 0, len(c.Rules)+len(rules))
	kept = append(kept, head...)
	kept = append(kept, rules...)
	c.Rules = append(kept, last)
}
