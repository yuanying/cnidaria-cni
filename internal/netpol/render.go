package netpol

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/yuanying/cnidaria-cni/internal/nftables"
)

// Add renders the NetworkPolicies into the table nftables.Render produced, in the
// five steps ADR 0003 describes: the pods a policy selects go into the isolated set
// for each direction it governs, the policy gets a chain of accepts per direction,
// and the dispatch chain gets a jump into that chain ahead of its drop.
//
// It is a pure function of p: no client, no kernel, no clock. A policy it cannot
// render is an error and the caller keeps the table the node already has, rather than
// applying half of what was asked for.
func Add(r *nftables.Ruleset, p Params) error {
	b := builder{
		ruleset:    r,
		nodeName:   p.NodeName,
		namespaces: make(map[string]Namespace, len(p.Namespaces)),
		isolated: map[PolicyType]map[netip.Addr]bool{
			PolicyTypeIngress: {},
			PolicyTypeEgress:  {},
		},
	}
	for _, ns := range p.Namespaces {
		b.namespaces[ns.Name] = ns
	}
	b.pods = usablePods(p.Pods)

	policies := slices.Clone(p.Policies)
	slices.SortFunc(policies, func(a, b NetworkPolicy) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	for _, policy := range policies {
		if err := b.policy(policy); err != nil {
			return fmt.Errorf("netpol: NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
	}
	return b.fillIsolated()
}

// builder carries what every step needs: the table being added to, the pods and
// namespaces to select from, and the addresses isolated so far.
type builder struct {
	ruleset    *nftables.Ruleset
	nodeName   string
	namespaces map[string]Namespace
	pods       []Pod
	isolated   map[PolicyType]map[netip.Addr]bool
}

// dir is one direction. Ingress and egress differ only in these values, so what
// follows is written once.
var (
	ingress = dir{
		typ: PolicyTypeIngress, tag: "in", chainPrefix: "ingress_",
		dispatch: nftables.ChainIngressDispatch,
		peerAddr: "saddr", selfAddr: "daddr",
		isolatedV4: nftables.SetIsolatedIngressV4, isolatedV6: nftables.SetIsolatedIngressV6,
	}
	egress = dir{
		typ: PolicyTypeEgress, tag: "eg", chainPrefix: "egress_",
		dispatch: nftables.ChainEgressDispatch,
		peerAddr: "daddr", selfAddr: "saddr",
		isolatedV4: nftables.SetIsolatedEgressV4, isolatedV6: nftables.SetIsolatedEgressV6,
	}
)

type dir struct {
	typ         PolicyType
	tag         string // the direction inside a set name
	chainPrefix string
	dispatch    string
	// peerAddr is the address a "from" or "to" entry names, selfAddr the address of
	// the pod the policy governs.
	peerAddr, selfAddr     string
	isolatedV4, isolatedV6 string
}

// usablePods are the pods a policy can name: not on the host network, not finished,
// and holding an address of their own.
func usablePods(pods []Pod) []Pod {
	out := make([]Pod, 0, len(pods))
	for _, pod := range pods {
		if pod.HostNetwork || pod.Terminated {
			continue
		}
		addrs := make([]netip.Addr, 0, len(pod.IPs))
		for _, ip := range pod.IPs {
			if ip.IsValid() {
				addrs = append(addrs, ip.Unmap())
			}
		}
		if len(addrs) == 0 {
			continue
		}
		pod.IPs = addrs
		out = append(out, pod)
	}
	slices.SortFunc(out, func(a, b Pod) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return out
}

// policy renders one NetworkPolicy. A policy that selects no pod of this node adds
// nothing: the pods it governs are enforced on the nodes they run on.
func (b *builder) policy(policy NetworkPolicy) error {
	selector, err := asSelector(&policy.PodSelector)
	if err != nil {
		return err
	}
	var selected []Pod
	for _, pod := range b.pods {
		if pod.Namespace == policy.Namespace && pod.NodeName == b.nodeName && selector.Matches(labels.Set(pod.Labels)) {
			selected = append(selected, pod)
		}
	}
	if len(selected) == 0 {
		return nil
	}

	comment := policy.Namespace + "/" + policy.Name
	selectedAddrs, err := b.podSets("pods_", []string{policy.Namespace, policy.Name}, selected)
	if err != nil {
		return err
	}
	for _, d := range []dir{ingress, egress} {
		if !policy.governs(d.typ) {
			continue
		}
		chain, err := nftables.Identifier(d.chainPrefix, policy.Namespace, policy.Name)
		if err != nil {
			return err
		}
		rules, err := b.directionRules(policy, d, selected)
		if err != nil {
			return err
		}
		b.ruleset.Chains = append(b.ruleset.Chains, nftables.Chain{
			Name:    chain,
			Comment: fmt.Sprintf("NetworkPolicy %s, %s", comment, d.typ),
			Rules:   rules,
		})

		var jumps []nftables.Rule
		for _, m := range selectedAddrs.matches(func(family, set string) string {
			return family + " " + d.selfAddr + " @" + set
		}) {
			jumps = append(jumps, nftables.Rule{Match: m.expr, Verdict: "jump " + chain, Comment: comment})
		}
		// Looked up after the sets and the chain were appended, since appending
		// invalidates an earlier pointer into the table.
		dispatch := b.ruleset.Chain(d.dispatch)
		if dispatch == nil {
			return fmt.Errorf("the table has no chain %s", d.dispatch)
		}
		dispatch.InsertBeforeLast(jumps...)

		for _, pod := range selected {
			for _, ip := range pod.IPs {
				b.isolated[d.typ][ip] = true
			}
		}
	}
	return nil
}

// directionRules turns the entries of one direction into accepts: for each entry, the
// cross product of the peers it names and the ports it opens. A combination whose two
// halves are about different address families is not a rule.
func (b *builder) directionRules(policy NetworkPolicy, d dir, selected []Pod) ([]nftables.Rule, error) {
	comment := policy.Namespace + "/" + policy.Name
	var out []nftables.Rule
	for i, rule := range policy.rules(d.typ) {
		peers := rule.Peers
		if len(peers) == 0 {
			// An empty "from" or "to" is every peer.
			peers = []Peer{{}}
		}
		for j, peer := range peers {
			peerMatches, peerPods, err := b.peer(policy, d, i, j, peer)
			if err != nil {
				return nil, err
			}
			// A named port is a port of whichever pod receives the traffic: the
			// pods the policy selects on ingress, the pods the entry names on
			// egress.
			receivers := selected
			if d.typ == PolicyTypeEgress {
				receivers = peerPods
			}
			portMatches, err := b.ports(policy, d, i, j, rule.Ports, receivers)
			if err != nil {
				return nil, err
			}
			for _, pm := range peerMatches {
				for _, qm := range portMatches {
					if m, ok := pm.and(qm); ok {
						out = append(out, nftables.Rule{Match: m.expr, Verdict: "accept", Comment: comment})
					}
				}
			}
		}
	}
	return out, nil
}

// peer turns one "from" or "to" entry into the matches that recognise it, one per
// address family it covers, and returns the pods it names so that a named port in the
// same entry can be resolved against them.
func (b *builder) peer(policy NetworkPolicy, d dir, i, j int, peer Peer) ([]match, []Pod, error) {
	switch {
	case peer.IPBlock != nil:
		// An ipBlock names addresses, which need not be pods, so it returns none.
		// A named port in the same entry then resolves against nothing and the
		// entry produces no rule at all, which is the semantics: a port named
		// rather than numbered is a port some pod declared.
		return []match{ipBlockMatch(d, *peer.IPBlock)}, nil, nil
	case peer.PodSelector == nil && peer.NamespaceSelector == nil:
		// Every peer, pods and the world outside the cluster alike.
		return []match{{}}, b.pods, nil
	}

	pods, err := b.peerPods(policy.Namespace, peer)
	if err != nil {
		return nil, nil, err
	}
	name := d.tag + strconv.Itoa(i) + "." + strconv.Itoa(j)
	addrs, err := b.podSets("peer_", []string{policy.Namespace, policy.Name, name}, pods)
	if err != nil {
		return nil, nil, err
	}
	return addrs.matches(func(family, set string) string {
		return family + " " + d.peerAddr + " @" + set
	}), pods, nil
}

// peerPods are the pods one selector entry names. Within the entry the two selectors
// are an AND; a missing namespaceSelector keeps the entry in the policy's own
// namespace, and a missing podSelector takes every pod of the namespaces it covers.
func (b *builder) peerPods(policyNamespace string, peer Peer) ([]Pod, error) {
	podSelector := labels.Everything()
	if peer.PodSelector != nil {
		var err error
		if podSelector, err = asSelector(peer.PodSelector); err != nil {
			return nil, err
		}
	}
	covered := map[string]bool{policyNamespace: true}
	if peer.NamespaceSelector != nil {
		namespaceSelector, err := asSelector(peer.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		covered = map[string]bool{}
		for name, ns := range b.namespaces {
			if namespaceSelector.Matches(labels.Set(ns.Labels)) {
				covered[name] = true
			}
		}
	}
	var out []Pod
	for _, pod := range b.pods {
		if covered[pod.Namespace] && podSelector.Matches(labels.Set(pod.Labels)) {
			out = append(out, pod)
		}
	}
	return out, nil
}

// ipBlockMatch recognises an address range. The ranges cut out of it are a second,
// negated match on the same address rather than a range subtracted from a set: what
// this entry excludes stays open to another entry and to another policy, which is
// what "allowed if any rule allows" means.
func ipBlockMatch(d dir, block IPBlock) match {
	fam, prefix := familyV4, "ip"
	if block.CIDR.Addr().Is6() {
		fam, prefix = familyV6, "ip6"
	}
	expr := prefix + " " + d.peerAddr + " " + block.CIDR.String()
	if len(block.Except) > 0 {
		except := make([]string, 0, len(block.Except))
		for _, e := range block.Except {
			except = append(except, e.String())
		}
		expr += fmt.Sprintf(" %s %s != { %s }", prefix, d.peerAddr, strings.Join(except, ", "))
	}
	return match{fam: fam, expr: expr}
}

// ports turns a "ports" list into the matches that recognise it. No ports at all is
// every port, which is a match on nothing.
func (b *builder) ports(policy NetworkPolicy, d dir, i, j int, ports []Port, receivers []Pod) ([]match, error) {
	if len(ports) == 0 {
		return []match{{}}, nil
	}
	var out []match
	for k, port := range ports {
		protocol := strings.ToLower(string(port.Protocol))
		if port.Protocol == "" {
			protocol = strings.ToLower(string(ProtocolTCP))
		}
		switch {
		case port.Name != "":
			named, err := b.namedPort(policy, d, i, j, k, port, protocol, receivers)
			if err != nil {
				return nil, err
			}
			out = append(out, named...)
		case port.Number == 0:
			// The API's "port" was left out: every port of the protocol.
			out = append(out, match{expr: "meta l4proto " + protocol})
		case port.EndPort > port.Number:
			out = append(out, match{expr: fmt.Sprintf("%s dport %d-%d", protocol, port.Number, port.EndPort)})
		case port.EndPort != 0 && port.EndPort < port.Number:
			return nil, fmt.Errorf("endPort %d is below port %d", port.EndPort, port.Number)
		default:
			out = append(out, match{expr: fmt.Sprintf("%s dport %d", protocol, port.Number)})
		}
	}
	return out, nil
}

// namedPort resolves a port name against the pods that receive the traffic. Two pods
// may give one name two numbers, or one of them may not declare it at all, so each
// number comes with the addresses of the pods that gave it: the rule opens that number
// on those pods and on no others.
func (b *builder) namedPort(policy NetworkPolicy, d dir, i, j, k int, port Port, protocol string, receivers []Pod) ([]match, error) {
	byNumber := map[uint16][]Pod{}
	for _, pod := range receivers {
		for _, declared := range pod.Ports {
			if declared.Name != port.Name {
				continue
			}
			if !samePortProtocol(declared.Protocol, port.Protocol) {
				continue
			}
			byNumber[declared.Port] = append(byNumber[declared.Port], pod)
		}
	}
	numbers := make([]uint16, 0, len(byNumber))
	for number := range byNumber {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)

	name := d.tag + strconv.Itoa(i) + "." + strconv.Itoa(j) + "." + strconv.Itoa(k)
	var out []match
	for _, number := range numbers {
		parts := []string{policy.Namespace, policy.Name, name, strconv.Itoa(int(number))}
		addrs, err := b.podSets("port_", parts, byNumber[number])
		if err != nil {
			return nil, err
		}
		out = append(out, addrs.matches(func(family, set string) string {
			return fmt.Sprintf("%s daddr @%s %s dport %d", family, set, protocol, number)
		})...)
	}
	return out, nil
}

// samePortProtocol compares a container port's protocol with a policy entry's, both
// of which default to TCP when empty.
func samePortProtocol(a, b Protocol) bool {
	if a == "" {
		a = ProtocolTCP
	}
	if b == "" {
		b = ProtocolTCP
	}
	return a == b
}

// podAddrs are the sets one group of pods became, one name per family. An empty name
// is a family no pod in the group has an address for: no set was declared, because an
// empty set is a rule that cannot match and is better not written at all.
type podAddrs struct{ v4, v6 string }

// matches builds the rule conditions that read those sets, one per family there is a
// set for. expr is handed the nft keyword for the family and the set's name; every
// rule that reads a group of pods is built this way, so a family is never quietly
// left out of one of them.
func (a podAddrs) matches(expr func(family, set string) string) []match {
	var out []match
	if a.v4 != "" {
		out = append(out, match{fam: familyV4, expr: expr("ip", a.v4)})
	}
	if a.v6 != "" {
		out = append(out, match{fam: familyV6, expr: expr("ip6", a.v6)})
	}
	return out
}

// podSets declares one set per address family for a group of pods and returns their
// names.
func (b *builder) podSets(prefix string, parts []string, pods []Pod) (podAddrs, error) {
	var addrs4, addrs6 []string
	for _, pod := range pods {
		for _, ip := range pod.IPs {
			if ip.Is4() {
				addrs4 = append(addrs4, ip.String())
			} else {
				addrs6 = append(addrs6, ip.String())
			}
		}
	}
	addSet := func(addrs []string, typ, family string) (string, error) {
		if len(addrs) == 0 {
			return "", nil
		}
		slices.Sort(addrs)
		name, err := nftables.Identifier(prefix, append(slices.Clone(parts), family)...)
		if err != nil {
			return "", err
		}
		b.ruleset.Sets = append(b.ruleset.Sets, nftables.Set{
			Name: name, Type: typ, Elements: slices.Compact(addrs),
		})
		return name, nil
	}

	var out podAddrs
	var err error
	if out.v4, err = addSet(addrs4, "ipv4_addr", "v4"); err != nil {
		return podAddrs{}, err
	}
	if out.v6, err = addSet(addrs6, "ipv6_addr", "v6"); err != nil {
		return podAddrs{}, err
	}
	return out, nil
}

// fillIsolated puts the addresses of every pod a policy selected into the set for its
// direction. A pod in neither set is never sent to a dispatch chain, which is the
// "everything is allowed" default (ADR 0003).
func (b *builder) fillIsolated() error {
	for _, d := range []dir{ingress, egress} {
		set4, set6 := b.ruleset.Set(d.isolatedV4), b.ruleset.Set(d.isolatedV6)
		if set4 == nil || set6 == nil {
			return fmt.Errorf("netpol: the table has no sets %s and %s", d.isolatedV4, d.isolatedV6)
		}
		addrs := make([]netip.Addr, 0, len(b.isolated[d.typ]))
		for addr := range b.isolated[d.typ] {
			addrs = append(addrs, addr)
		}
		slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
		for _, addr := range addrs {
			if addr.Is4() {
				set4.Elements = append(set4.Elements, addr.String())
			} else {
				set6.Elements = append(set6.Elements, addr.String())
			}
		}
	}
	return nil
}

// asSelector reads a label selector, failing on one the API server would have
// rejected rather than letting it quietly select nothing.
func asSelector(s *metav1.LabelSelector) (labels.Selector, error) {
	selector, err := metav1.LabelSelectorAsSelector(s)
	if err != nil {
		return nil, fmt.Errorf("label selector: %w", err)
	}
	return selector, nil
}

// family is the address family a match is about. A match that names no address works
// for both.
type family int

const (
	familyBoth family = iota
	familyV4
	familyV6
)

// match is one condition of a rule together with the family it constrains. An empty
// expression matches everything.
type match struct {
	fam  family
	expr string
}

// and joins two matches into one. Two matches about different families are not a
// rule: an entry naming IPv4 peers and a port that only IPv6 pods carry recognises
// nothing, and writing it would be a rule nft rejects.
func (m match) and(other match) (match, bool) {
	fam := m.fam
	switch {
	case m.fam == other.fam:
	case m.fam == familyBoth:
		fam = other.fam
	case other.fam == familyBoth:
	default:
		return match{}, false
	}
	switch {
	case m.expr == "":
		return match{fam: fam, expr: other.expr}, true
	case other.expr == "":
		return match{fam: fam, expr: m.expr}, true
	}
	return match{fam: fam, expr: m.expr + " " + other.expr}, true
}
