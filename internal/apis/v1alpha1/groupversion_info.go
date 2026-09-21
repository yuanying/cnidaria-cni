package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version of everything in this package. The group sits
// under a domain the project controls and does not carry the name of the binary, so
// that renaming the binary does not move the API (ADR 0004).
var GroupVersion = schema.GroupVersion{Group: "cnidaria.unstable.cloud", Version: "v1alpha1"}

// SchemeBuilder registers these types with a scheme; the daemon's manager reads the
// API through a scheme that has them (ADR 0007). It is apimachinery's builder rather
// than controller-runtime's so that this package stays importable without pulling the
// controller machinery in behind it.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the types of this group version to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &NodeNetworkPolicy{}, &NodeNetworkPolicyList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
