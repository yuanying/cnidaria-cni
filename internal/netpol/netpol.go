package netpol

import (
	"net/netip"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The input is what the API objects say and nothing else: the fields of Pod,
// Namespace and NetworkPolicy the semantics depend on, copied into values this
// package can be handed in a test. The one type borrowed whole is
// metav1.LabelSelector, together with the matching in k8s.io/apimachinery's labels
// package. Its semantics — matchLabels, the four matchExpressions operators, an empty
// selector meaning everything — are the ones a policy author expects, and a second
// implementation of them here would only be a chance to get them subtly wrong. Both
// are plain libraries: no client, no cluster, nothing to reach for at run time.

// Params is what the NetworkPolicy part of one node's table is rendered from. Pods
// and Namespaces are the whole cluster's, since a policy may name a peer on any node,
// while only this node's own pods are enforced here: the node a packet is forwarded
// from applies the source pod's egress, and the node it is forwarded to applies the
// destination pod's ingress (ADR 0003).
type Params struct {
	NodeName   string
	Pods       []Pod
	Namespaces []Namespace
	Policies   []NetworkPolicy
}

// Pod is what a pod says that policy depends on.
type Pod struct {
	Namespace, Name string
	// NodeName is the node the pod runs on: spec.nodeName.
	NodeName string
	Labels   map[string]string
	// IPs are status.podIPs, at most one per family. A pod that has none has not
	// been given an address yet and is ignored until it has one.
	IPs []netip.Addr
	// HostNetwork pods share the node's addresses. A policy that named one would
	// name the node, so they are left out of selections entirely.
	HostNetwork bool
	// Terminated is a pod that has run to its end, Succeeded or Failed. It keeps
	// its addresses in the API until something deletes it, and by then one of them
	// may already belong to a pod that is running, so letting it stay in a set
	// would grant that new pod whatever the dead one's labels asked for.
	Terminated bool
	// Ports are the container ports the pod gives a name to. Only those matter: a
	// policy can reach a port by name only through one of them.
	Ports []NamedPort
}

// NamedPort is one named container port of a pod.
type NamedPort struct {
	Name     string
	Port     uint16
	Protocol Protocol
}

// Namespace is what a namespace says that policy depends on.
type Namespace struct {
	Name   string
	Labels map[string]string
}

// NetworkPolicy is one NetworkPolicy object.
type NetworkPolicy struct {
	Namespace, Name string
	// PodSelector chooses the pods in the policy's own namespace that it governs.
	// An empty selector is every pod in that namespace.
	PodSelector metav1.LabelSelector
	// PolicyTypes are the directions the policy isolates its pods for. Empty means
	// the default the API applies: Ingress always, Egress only if Egress is set.
	PolicyTypes []PolicyType
	Ingress     []Rule
	Egress      []Rule
}

// Rule is one entry of spec.ingress or spec.egress. Ingress rules name their peers in
// "from" and egress rules in "to"; the two are the same shape, so one type serves
// both. No peers means every peer, and no ports means every port.
type Rule struct {
	Peers []Peer
	Ports []Port
}

// Peer is one entry of a rule's "from" or "to". Either IPBlock is set, or one or both
// of the selectors: within one peer they are an AND, and separate peers are an OR.
// A peer with nothing set at all is every peer.
type Peer struct {
	// PodSelector chooses among the pods of the namespaces the peer covers, which
	// is the policy's own namespace unless NamespaceSelector says otherwise.
	PodSelector *metav1.LabelSelector
	// NamespaceSelector chooses the namespaces the peer covers. An empty selector
	// is every namespace.
	NamespaceSelector *metav1.LabelSelector
	IPBlock           *IPBlock
}

// IPBlock is a range of addresses outside the selector world, less the ranges cut out
// of it.
type IPBlock struct {
	CIDR   netip.Prefix
	Except []netip.Prefix
}

// Port is one entry of a rule's "ports".
type Port struct {
	// Protocol is TCP when empty, as it is in the API.
	Protocol Protocol
	// Number and Name are the two forms the API's "port" takes. Both empty opens
	// every port of the protocol.
	Number uint16
	Name   string
	// EndPort turns Number into the range Number..EndPort. The API allows it only
	// with a numeric port.
	EndPort uint16
}

// Protocol is the transport protocol of a port entry.
type Protocol string

// The protocols NetworkPolicy allows.
const (
	ProtocolTCP  Protocol = "TCP"
	ProtocolUDP  Protocol = "UDP"
	ProtocolSCTP Protocol = "SCTP"
)

// PolicyType is a direction a policy isolates its pods for.
type PolicyType string

// The directions.
const (
	PolicyTypeIngress PolicyType = "Ingress"
	PolicyTypeEgress  PolicyType = "Egress"
)

// governs reports whether the policy isolates its pods for a direction, applying the
// default the API applies when policyTypes is left out: a policy always governs
// ingress, and governs egress only when it declares egress rules.
func (p NetworkPolicy) governs(t PolicyType) bool {
	if len(p.PolicyTypes) == 0 {
		if t == PolicyTypeEgress {
			return len(p.Egress) > 0
		}
		return t == PolicyTypeIngress
	}
	for _, have := range p.PolicyTypes {
		if have == t {
			return true
		}
	}
	return false
}

// rules are the entries of the direction.
func (p NetworkPolicy) rules(t PolicyType) []Rule {
	if t == PolicyTypeEgress {
		return p.Egress
	}
	return p.Ingress
}
