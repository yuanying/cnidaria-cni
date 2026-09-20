package nftables

import "testing"

func TestSetAndChainLookup(t *testing.T) {
	r, err := Render(goldenCases[0].params)
	if err != nil {
		t.Fatal(err)
	}
	if s := r.Set(SetIsolatedIngressV4); s == nil {
		t.Fatalf("no set %s", SetIsolatedIngressV4)
	}
	if c := r.Chain(ChainEgressDispatch); c == nil {
		t.Fatalf("no chain %s", ChainEgressDispatch)
	}
	if r.Set("nothing") != nil || r.Chain("nothing") != nil {
		t.Error("a name the table does not hold returned something")
	}
}

// The drop at the end of a dispatch chain has to stay last, whatever is put in front
// of it (ADR 0003).
func TestInsertBeforeLastKeepsTheDropLast(t *testing.T) {
	c := Chain{Rules: []Rule{{Verdict: "drop"}}}
	c.InsertBeforeLast(Rule{Verdict: "jump a"})
	c.InsertBeforeLast(Rule{Verdict: "jump b"}, Rule{Verdict: "jump c"})

	var got []string
	for _, rule := range c.Rules {
		got = append(got, rule.Verdict)
	}
	want := []string{"jump a", "jump b", "jump c", "drop"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestInsertBeforeLastOnAnEmptyChainAppends(t *testing.T) {
	c := Chain{}
	c.InsertBeforeLast(Rule{Verdict: "accept"})
	if len(c.Rules) != 1 || c.Rules[0].Verdict != "accept" {
		t.Errorf("got %+v", c.Rules)
	}
}
