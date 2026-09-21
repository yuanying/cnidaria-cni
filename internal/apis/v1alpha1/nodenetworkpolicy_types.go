package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mode decides what happens to a packet no rule of a policy accepted.
//
// +kubebuilder:validation:Enum=Permissive;Enforce
type Mode string

const (
	// ModePermissive drops nothing: what Enforce would drop is logged and counted
	// instead, so the whole effect of a policy is visible before it has any.
	ModePermissive Mode = "Permissive"
	// ModeEnforce drops what no rule accepted.
	ModeEnforce Mode = "Enforce"
)

// PolicyType is a direction a policy closes.
//
// +kubebuilder:validation:Enum=Ingress;Egress
type PolicyType string

const (
	// PolicyTypeIngress is traffic arriving at the node's own sockets.
	PolicyTypeIngress PolicyType = "Ingress"
	// PolicyTypeEgress is traffic the node itself sends.
	PolicyTypeEgress PolicyType = "Egress"
)

// NodeNetworkPolicy governs what may reach and what may leave the node itself, which is the
// traffic NetworkPolicy does not describe (ADR 0004). It is cluster-scoped, selects
// nodes by label, and drops nothing until an operator asks for Enforce.
//
// A node selected by no policy of a direction is open in that direction; a node
// selected by one accepts only what its rules list. Several policies selecting one
// node are unioned. The rules that keep a node reachable whatever a policy says —
// established traffic, loopback, the ICMP types a host needs, SSH, kubelet, the API
// server, etcd and the NodePort range — sit ahead of every policy and no field here
// turns them off.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nnp
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type NodeNetworkPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeNetworkPolicySpec   `json:"spec,omitempty"`
	Status NodeNetworkPolicyStatus `json:"status,omitempty"`
}

// NodeNetworkPolicyList is a list of NodeNetworkPolicy.
//
// +kubebuilder:object:root=true
type NodeNetworkPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeNetworkPolicy `json:"items"`
}

// NodeNetworkPolicySpec is the policy itself. Its vocabulary is NetworkPolicy's on purpose:
// an operator who can write one can write the other.
type NodeNetworkPolicySpec struct {
	// mode is Permissive (the default) or Enforce. A node enforces a direction only
	// when every policy that closes that direction on it asks for Enforce, so one
	// permissive policy keeps the node observing.
	//
	// +kubebuilder:default=Permissive
	// +optional
	Mode Mode `json:"mode,omitempty"`

	// nodeSelector chooses the nodes this policy applies to. An empty selector
	// selects every node.
	//
	// +optional
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`

	// policyTypes says which directions this policy closes. Left out, it follows
	// NetworkPolicy's rule: Ingress always, Egress as well when egress rules are
	// present.
	//
	// +optional
	PolicyTypes []PolicyType `json:"policyTypes,omitempty"`

	// ingress lists what a selected node accepts. An entry with no peers accepts
	// from anywhere, and one with no ports accepts on every port.
	//
	// +optional
	Ingress []NodeNetworkPolicyIngressRule `json:"ingress,omitempty"`

	// egress lists what a selected node may send, with the same defaults as ingress.
	//
	// +optional
	Egress []NodeNetworkPolicyEgressRule `json:"egress,omitempty"`
}

// NodeNetworkPolicyIngressRule allows traffic that matches one of its peers and one of its
// ports.
type NodeNetworkPolicyIngressRule struct {
	// from are the sources this rule allows. Empty allows every source.
	//
	// +optional
	From []NodeNetworkPolicyPeer `json:"from,omitempty"`

	// ports are the ports on the node this rule allows. Empty allows every port.
	//
	// +optional
	Ports []NodeNetworkPolicyPort `json:"ports,omitempty"`
}

// NodeNetworkPolicyEgressRule allows traffic that matches one of its peers and one of its
// ports.
type NodeNetworkPolicyEgressRule struct {
	// to are the destinations this rule allows. Empty allows every destination.
	//
	// +optional
	To []NodeNetworkPolicyPeer `json:"to,omitempty"`

	// ports are the destination ports this rule allows. Empty allows every port.
	//
	// +optional
	Ports []NodeNetworkPolicyPort `json:"ports,omitempty"`
}

// NodeNetworkPolicyPeer is the other end of the traffic. Pod and namespace selectors are not
// peers here: a node is addressed by the network, not by the cluster (ADR 0004). Pods
// are reached through their CIDRs like any other address.
type NodeNetworkPolicyPeer struct {
	// ipBlock is a CIDR, with optional exceptions inside it.
	IPBlock *networkingv1.IPBlock `json:"ipBlock"`
}

// NodeNetworkPolicyPort is one protocol and one port or range of ports. Ports are numbers
// here and never names: a node has no container ports for a name to refer to.
//
// What a range has to hold is stated to the API server as well as checked when the
// rule is rendered, so that an object the node cannot render is refused by kubectl
// apply rather than accepted and then reported back in status.
//
// +kubebuilder:validation:XValidation:rule="!has(self.endPort) || has(self.port)",message="endPort needs a port"
// +kubebuilder:validation:XValidation:rule="!has(self.endPort) || !has(self.port) || self.endPort >= self.port",message="endPort must not be below port"
type NodeNetworkPolicyPort struct {
	// protocol is TCP, UDP or SCTP. It defaults to TCP.
	//
	// +kubebuilder:default=TCP
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`

	// port is the port, or the first port of the range when endPort is set. Left
	// out, the rule covers every port of the protocol.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// endPort, together with port, makes the rule cover port through endPort
	// inclusive. It may not be given without port and may not be below it.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	EndPort *int32 `json:"endPort,omitempty"`
}

// NodeNetworkPolicyStatus is what the nodes report back.
type NodeNetworkPolicyStatus struct {
	// nodes holds one entry per node that has applied this policy. Each selected
	// node writes its own entry and leaves the others alone.
	//
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []NodeNetworkPolicyNodeStatus `json:"nodes,omitempty"`
}

// NodeNetworkPolicyNodeStatus is one node's report.
type NodeNetworkPolicyNodeStatus struct {
	// name is the node's name.
	Name string `json:"name"`

	// observedGeneration is the metadata.generation this node last rendered.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// mode is the mode that generation took effect in on this node. A policy asking
	// for Enforce reads Permissive here for as long as another permissive policy
	// shares a direction with it.
	//
	// +optional
	Mode Mode `json:"mode,omitempty"`

	// message says why the policy could not be rendered on this node, and is empty
	// when it could.
	//
	// +optional
	Message string `json:"message,omitempty"`
}
