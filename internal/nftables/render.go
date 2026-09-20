package nftables

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Params is everything the table for one node depends on, before any policy is
// rendered into it. Rendering is a function of these values alone: it runs no command
// and reads no kernel state (ADR 0003).
type Params struct {
	// PodCIDRs are this node's node.spec.podCIDRs: at most one prefix per family.
	PodCIDRs []netip.Prefix
	// ClusterPodCIDRs are every node's podCIDRs, this node's included. Traffic from
	// this node's pods to any of them stays inside the cluster and is not masqueraded.
	ClusterPodCIDRs []netip.Prefix
	// SafePorts are the ports the rules a NodePolicy cannot remove keep open (ADR 0004).
	SafePorts SafePorts
}

// SafePorts are the ports of the safe rules. They are arguments to the daemon because
// a cluster may run its API server or its NodePort range elsewhere (ADR 0004).
type SafePorts struct {
	SSH       uint16
	Kubelet   uint16
	APIServer uint16
	Etcd      PortRange
	NodePort  PortRange
	DNS       uint16
}

// DefaultSafePorts are the conventional values.
var DefaultSafePorts = SafePorts{
	SSH:       22,
	Kubelet:   10250,
	APIServer: 6443,
	Etcd:      PortRange{From: 2379, To: 2380},
	NodePort:  PortRange{From: 30000, To: 32767},
	DNS:       53,
}

// PortRange is an inclusive range of ports. From equal to To is a single port.
type PortRange struct {
	From, To uint16
}

func (r PortRange) String() string {
	if r.From == r.To {
		return strconv.Itoa(int(r.From))
	}
	return fmt.Sprintf("%d-%d", r.From, r.To)
}

// The names of the sets and chains Render declares. The NetworkPolicy renderer fills
// the isolated sets and puts its jumps into the dispatch chains; the NodePolicy renderer
// appends to input and output after the safe rules.
const (
	SetNodePodsV4        = "node_pods_v4"
	SetNodePodsV6        = "node_pods_v6"
	SetClusterPodsV4     = "cluster_pods_v4"
	SetClusterPodsV6     = "cluster_pods_v6"
	SetIsolatedEgressV4  = "isolated_egress_v4"
	SetIsolatedEgressV6  = "isolated_egress_v6"
	SetIsolatedIngressV4 = "isolated_ingress_v4"
	SetIsolatedIngressV6 = "isolated_ingress_v6"

	ChainEgress          = "egress"
	ChainEgressDispatch  = "egress_dispatch"
	ChainIngress         = "ingress"
	ChainIngressDispatch = "ingress_dispatch"
	ChainInput           = "input"
	ChainOutput          = "output"
	ChainPostrouting     = "postrouting"
)

// The ICMP types the safe rules let through (ADR 0004): what path MTU discovery,
// neighbour discovery and address resolution need. Without them an IPv6 node vanishes
// from its own segment.
const (
	safeICMPTypes   = "echo-request, destination-unreachable, time-exceeded, parameter-problem"
	safeICMPv6Types = safeICMPTypes + ", packet-too-big, " +
		"nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert, " +
		"mld-listener-query, mld-listener-report, mld-listener-done, mld2-listener-report"
)

// Render builds the skeleton of the table: the sets for the pod CIDRs, the five base
// chains, the empty dispatch chains, the safe rules and the masquerade (ADR 0003).
func Render(p Params) (*Ruleset, error) {
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("nftables: %w", err)
	}
	nodeV4, nodeV6 := splitFamilies(p.PodCIDRs)
	clusterV4, clusterV6 := splitFamilies(p.ClusterPodCIDRs)

	r := &Ruleset{}
	r.Sets = []Set{
		prefixSet(SetNodePodsV4, "ipv4_addr", nodeV4, "This node's pod CIDRs (node.spec.podCIDRs)"),
		prefixSet(SetNodePodsV6, "ipv6_addr", nodeV6, ""),
		prefixSet(SetClusterPodsV4, "ipv4_addr", clusterV4,
			"Every node's pod CIDRs: traffic to these stays inside the cluster and is not masqueraded"),
		prefixSet(SetClusterPodsV6, "ipv6_addr", clusterV6, ""),
		{Name: SetIsolatedEgressV4, Type: "ipv4_addr",
			Comment: "Pods selected by at least one NetworkPolicy of that policyType; filled by the NetworkPolicy renderer"},
		{Name: SetIsolatedEgressV6, Type: "ipv6_addr"},
		{Name: SetIsolatedIngressV4, Type: "ipv4_addr"},
		{Name: SetIsolatedIngressV6, Type: "ipv6_addr"},
	}
	r.Chains = []Chain{
		podChain(ChainEgress, "filter", "saddr", SetIsolatedEgressV4, SetIsolatedEgressV6, ChainEgressDispatch,
			"NetworkPolicy egress, keyed on the source pod (ADR 0003)"),
		dispatchChain(ChainEgressDispatch),
		podChain(ChainIngress, "filter + 1", "daddr", SetIsolatedIngressV4, SetIsolatedIngressV6, ChainIngressDispatch,
			"NetworkPolicy ingress, keyed on the destination pod (ADR 0003)"),
		dispatchChain(ChainIngressDispatch),
		inputChain(p.SafePorts),
		outputChain(p.SafePorts),
		postroutingChain(),
	}
	return r, nil
}

func (p Params) validate() error {
	if len(p.PodCIDRs) == 0 {
		return errors.New("at least one pod CIDR is required")
	}
	var sawV4, sawV6 bool
	for _, cidr := range p.PodCIDRs {
		if !cidr.IsValid() {
			return fmt.Errorf("invalid pod CIDR %q", cidr)
		}
		switch {
		case cidr.Addr().Is4() && !sawV4:
			sawV4 = true
		case cidr.Addr().Is6() && !sawV6:
			sawV6 = true
		default:
			return fmt.Errorf("more than one pod CIDR for one address family: %s", cidr)
		}
	}
	for _, cidr := range p.ClusterPodCIDRs {
		if !cidr.IsValid() {
			return fmt.Errorf("invalid cluster pod CIDR %q", cidr)
		}
	}
	for name, port := range map[string]uint16{
		"ssh": p.SafePorts.SSH, "kubelet": p.SafePorts.Kubelet,
		"apiserver": p.SafePorts.APIServer, "dns": p.SafePorts.DNS,
	} {
		if port == 0 {
			return fmt.Errorf("safe port %s is required", name)
		}
	}
	for name, ports := range map[string]PortRange{"etcd": p.SafePorts.Etcd, "nodeport": p.SafePorts.NodePort} {
		if ports.From == 0 || ports.To < ports.From {
			return fmt.Errorf("safe port range %s is empty: %s", name, ports)
		}
	}
	return nil
}

// splitFamilies sorts the prefixes into one list per family, in a fixed order with
// duplicates removed, so that the sets do not depend on the order nodes were listed in.
func splitFamilies(prefixes []netip.Prefix) (v4, v6 []string) {
	sorted := slices.Clone(prefixes)
	for i := range sorted {
		sorted[i] = sorted[i].Masked()
	}
	slices.SortFunc(sorted, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	for _, prefix := range slices.Compact(sorted) {
		if prefix.Addr().Is4() {
			v4 = append(v4, prefix.String())
		} else {
			v6 = append(v6, prefix.String())
		}
	}
	return v4, v6
}

func prefixSet(name, typ string, elements []string, comment string) Set {
	return Set{Name: name, Type: typ, Flags: []string{"interval"}, Elements: elements, Comment: comment}
}

// podChain is the forward-hook chain for one direction of NetworkPolicy: replies pass,
// then a pod in the isolated set for that direction is sent to the dispatch chain.
// Pods in no set are not evaluated at all, which is the "all allowed" default.
func podChain(name, priority, addr, setV4, setV6, dispatch, comment string) Chain {
	return Chain{
		Name:    name,
		Base:    &BaseChain{Type: "filter", Hook: "forward", Priority: priority, Policy: "accept"},
		Comment: comment,
		Rules: []Rule{
			{Match: "ct state established,related", Verdict: "accept", Comment: "replies follow the connection"},
			{Match: "ip " + addr + " @" + setV4, Verdict: "jump " + dispatch, Comment: "isolated for " + name},
			{Match: "ip6 " + addr + " @" + setV6, Verdict: "jump " + dispatch, Comment: "isolated for " + name},
		},
	}
}

// dispatchChain ends in a drop: an isolated pod for which no policy accepted is
// denied. The NetworkPolicy renderer puts one jump per policy ahead of it.
func dispatchChain(name string) Chain {
	return Chain{
		Name:    name,
		Comment: "One jump per NetworkPolicy goes ahead of the drop",
		Rules:   []Rule{{Verdict: "drop", Comment: "isolated pod, no policy accepted"}},
	}
}

// safeRulesCommon are the safe rules both node chains open with (ADR 0004). iface is
// "iifname" on input and "oifname" on output.
func safeRulesCommon(iface string) []Rule {
	return []Rule{
		{Match: "ct state established,related", Verdict: "accept", Comment: "safe: replies to what the node opened"},
		{Match: iface + ` "lo"`, Verdict: "accept", Comment: "safe: loopback"},
		{Match: "icmp type { " + safeICMPTypes + " }", Verdict: "accept", Comment: "safe: icmp"},
		{Match: "icmpv6 type { " + safeICMPv6Types + " }", Verdict: "accept", Comment: "safe: icmpv6"},
	}
}

func safePort(proto string, port fmt.Stringer, what string) Rule {
	return Rule{Match: proto + " dport " + port.String(), Verdict: "accept", Comment: "safe: " + what}
}

func single(port uint16) PortRange { return PortRange{From: port, To: port} }

func inputChain(ports SafePorts) Chain {
	rules := append(safeRulesCommon("iifname"),
		safePort("tcp", single(ports.SSH), "ssh"),
		safePort("tcp", single(ports.Kubelet), "kubelet"),
		safePort("tcp", single(ports.APIServer), "apiserver"),
		safePort("tcp", ports.Etcd, "etcd"),
		safePort("tcp", ports.NodePort, "nodeport"),
		safePort("udp", ports.NodePort, "nodeport"),
	)
	return Chain{
		Name:    ChainInput,
		Base:    &BaseChain{Type: "filter", Hook: "input", Priority: "filter", Policy: "accept"},
		Comment: "NodePolicy ingress, behind the safe rules a policy cannot remove (ADR 0004)",
		Rules:   rules,
	}
}

func outputChain(ports SafePorts) Chain {
	rules := append(safeRulesCommon("oifname"),
		safePort("tcp", single(ports.APIServer), "apiserver"),
		safePort("tcp", ports.Etcd, "etcd"),
		safePort("tcp", single(ports.Kubelet), "kubelet"),
		safePort("tcp", single(ports.DNS), "dns"),
		safePort("udp", single(ports.DNS), "dns"),
		Rule{Match: "ip daddr @" + SetNodePodsV4, Verdict: "accept", Comment: "safe: this node's pods"},
		Rule{Match: "ip6 daddr @" + SetNodePodsV6, Verdict: "accept", Comment: "safe: this node's pods"},
	)
	return Chain{
		Name:    ChainOutput,
		Base:    &BaseChain{Type: "filter", Hook: "output", Priority: "filter", Policy: "accept"},
		Comment: "NodePolicy egress, behind the safe rules a policy cannot remove (ADR 0004)",
		Rules:   rules,
	}
}

// postroutingChain masquerades what leaves this node's pods for anywhere outside the
// cluster's pod CIDRs. Plain masquerade, no flags: the source port is kept where it
// can be, so a pod's NAT mapping does not depend on who it is talking to (ADR 0003).
func postroutingChain() Chain {
	return Chain{
		Name:    ChainPostrouting,
		Base:    &BaseChain{Type: "nat", Hook: "postrouting", Priority: "srcnat", Policy: "accept"},
		Comment: "Masquerade for pod traffic leaving the cluster's pod CIDRs (ADR 0001, ADR 0003)",
		Rules: []Rule{
			{Match: "ip saddr @" + SetNodePodsV4 + " ip daddr != @" + SetClusterPodsV4,
				Verdict: "masquerade", Comment: "pod traffic leaving the cluster"},
			{Match: "ip6 saddr @" + SetNodePodsV6 + " ip6 daddr != @" + SetClusterPodsV6,
				Verdict: "masquerade", Comment: "pod traffic leaving the cluster"},
		},
	}
}

// What nft reads back as one identifier: letters, digits and _ . / - after a leading
// letter. The prefix supplies the leading letter, so only the characters of the parts
// have to hold. The kernel refuses a name of 256 bytes or more.
var identifierPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

const (
	identifierMaxLen = 255
	// How much of the hash a folded name ends in. Twelve hex digits of SHA-256 is
	// far more than the few hundred names one node holds needs.
	identifierHashLen = 12
)

// Identifier is the nftables name for the set or chain rendered from a Kubernetes
// object. prefix says what kind of thing it is; the object's namespace and name follow,
// joined with "/", which no Kubernetes name may contain, so two objects never share an
// identifier. A name nft could not read back is an error rather than text that would
// parse as something else (ADR 0003).
//
// A name too long for nft is folded rather than refused: a namespace and a name may
// each be up to 63 and 253 bytes, so the limit is reachable without anybody doing
// anything odd, and refusing it would stop the whole table of that node being updated
// until the policy was renamed.
func Identifier(prefix string, parts ...string) (string, error) {
	if len(parts) == 0 {
		return "", errors.New("nftables: an identifier needs a name")
	}
	for _, part := range parts {
		if !identifierPart.MatchString(part) {
			return "", fmt.Errorf("nftables: %q cannot be used in an nftables name: letters, digits, _ . - only", part)
		}
	}
	id := prefix + strings.Join(parts, "/")
	if len(id) > identifierMaxLen {
		id = fold(id)
	}
	return id, nil
}

// fold replaces the tail of an over-long name with a hash of the whole of it. The head
// that survives still says what the name came from, and the hash keeps two names that
// share it apart. It is a function of the name alone, so every node renders the same
// object to the same identifier and a reapply is the same text.
func fold(id string) string {
	sum := sha256.Sum256([]byte(id))
	tag := hex.EncodeToString(sum[:])[:identifierHashLen]
	return id[:identifierMaxLen-len(tag)-1] + "/" + tag
}
