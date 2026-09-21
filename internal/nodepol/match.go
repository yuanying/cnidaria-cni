package nodepol

import (
	"fmt"
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/yuanying/cnidaria-cni/internal/apis/v1alpha1"
)

// comment is what every rule rendered from a policy carries, so that "nft list
// ruleset" on the node reads in the operator's vocabulary (ADR 0003). A name long
// enough to pass what nft takes in a comment is cut down by the nftables package
// when the text is written; a policy is not refused over the length of its name.
func comment(name string) string { return "nodenetworkpolicy " + name }

// matches turns one ingress or egress rule into the match part of every nft rule it
// allows: each peer with each port. A rule with no peers allows every address and one
// with no ports allows every port, so an empty entry matches everything.
func (d direction) matches(e entry) ([]string, error) {
	peers, err := collect(e.peers, d.peerMatch)
	if err != nil {
		return nil, err
	}
	ports, err := collect(e.ports, portMatch)
	if err != nil {
		return nil, err
	}
	matches := make([]string, 0, len(peers)*len(ports))
	for _, peer := range peers {
		for _, port := range ports {
			matches = append(matches, strings.TrimSpace(peer+" "+port))
		}
	}
	return matches, nil
}

// collect renders each element, or returns the one empty match that stands for "any"
// when there is nothing to render.
func collect[T any](elements []T, render func(T) (string, error)) ([]string, error) {
	if len(elements) == 0 {
		return []string{""}, nil
	}
	out := make([]string, 0, len(elements))
	for _, e := range elements {
		match, err := render(e)
		if err != nil {
			return nil, err
		}
		out = append(out, match)
	}
	return out, nil
}

// peerMatch is the address part of a rule. The exceptions of an ipBlock become a
// negated anonymous set beside the prefix, which keeps the peer one rule.
func (d direction) peerMatch(peer v1alpha1.NodeNetworkPolicyPeer) (string, error) {
	if peer.IPBlock == nil {
		return "", fmt.Errorf("a peer needs an ipBlock")
	}
	block, err := prefix(peer.IPBlock.CIDR)
	if err != nil {
		return "", fmt.Errorf("ipBlock cidr: %w", err)
	}
	family := "ip"
	if block.Addr().Is6() {
		family = "ip6"
	}
	match := fmt.Sprintf("%s %s %s", family, d.addr, block)
	if len(peer.IPBlock.Except) == 0 {
		return match, nil
	}
	excepts := make([]string, 0, len(peer.IPBlock.Except))
	for _, e := range peer.IPBlock.Except {
		except, err := prefix(e)
		if err != nil {
			return "", fmt.Errorf("ipBlock except: %w", err)
		}
		if except.Addr().Is6() != block.Addr().Is6() {
			return "", fmt.Errorf("ipBlock except %s is not of the same family as cidr %s", except, block)
		}
		excepts = append(excepts, except.String())
	}
	return fmt.Sprintf("%s %s %s != { %s }", match, family, d.addr, strings.Join(excepts, ", ")), nil
}

// prefix reads one CIDR. A prefix carrying bits below its length, 192.0.2.1/24 for
// 192.0.2.0/24, is refused rather than quietly masked: the rule would then allow
// something other than what the object says, and the API server refuses the same
// thing in a NetworkPolicy.
func prefix(cidr string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q: %w", cidr, err)
	}
	if p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("%q has bits set below its prefix length; write %s", cidr, p.Masked())
	}
	return p, nil
}

// portMatch is the protocol and port part of a rule. A port entry that names only a
// protocol covers every port of it.
func portMatch(port v1alpha1.NodeNetworkPolicyPort) (string, error) {
	proto, err := protocol(port.Protocol)
	if err != nil {
		return "", err
	}
	if port.Port == nil {
		if port.EndPort != nil {
			return "", fmt.Errorf("endPort %d without a port", *port.EndPort)
		}
		return "meta l4proto " + proto, nil
	}
	first, err := portNumber(*port.Port)
	if err != nil {
		return "", err
	}
	if port.EndPort == nil {
		return fmt.Sprintf("%s dport %d", proto, first), nil
	}
	last, err := portNumber(*port.EndPort)
	if err != nil {
		return "", err
	}
	if last < first {
		return "", fmt.Errorf("endPort %d is below port %d", last, first)
	}
	return fmt.Sprintf("%s dport %d-%d", proto, first, last), nil
}

// protocol is nft's keyword for the protocol. An empty protocol is the TCP the CRD
// defaults to, for an object built in Go rather than read from the API.
func protocol(p corev1.Protocol) (string, error) {
	switch p {
	case "", corev1.ProtocolTCP:
		return "tcp", nil
	case corev1.ProtocolUDP:
		return "udp", nil
	case corev1.ProtocolSCTP:
		return "sctp", nil
	default:
		return "", fmt.Errorf("protocol %q is not one nftables matches on a port", p)
	}
}

func portNumber(p int32) (uint16, error) {
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d is outside 1-65535", p)
	}
	return uint16(p), nil
}
