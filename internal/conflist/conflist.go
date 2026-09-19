// Package conflist renders the CNI configuration list that cnidaria writes to the
// node. The list names only the reference plugins: bridge, host-local and portmap.
// cnidaria has no CNI binary of its own (ADR 0001), so this document is the whole of
// what it hands to the container runtime.
package conflist

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
)

// CNIVersion is the CNI specification version the rendered list declares.
const CNIVersion = "1.0.0"

// Params is everything the conflist for one node depends on.
type Params struct {
	// Name is the network name. host-local keys its lease directory by it.
	Name string
	// Bridge is the name of the Linux bridge the pods attach to.
	Bridge string
	// MTU is set on the bridge and every veth. Zero leaves it to the bridge plugin.
	MTU int
	// PodCIDRs are this node's node.spec.podCIDRs: at most one prefix per family.
	PodCIDRs []netip.Prefix
}

// Render returns the conflist as indented JSON ending with a newline.
//
// Each pod CIDR becomes one host-local range set, so a dual-stack node gets one IPv4
// and one IPv6 address per pod (ADR 0005). Two prefixes of the same family are
// refused: node.spec.podCIDRs never carries them and the bridge plugin's gateway
// handling assumes one per family.
func Render(p Params) ([]byte, error) {
	if p.Name == "" {
		return nil, errors.New("conflist: network name is required")
	}
	if p.Bridge == "" {
		return nil, errors.New("conflist: bridge name is required")
	}
	if len(p.PodCIDRs) == 0 {
		return nil, errors.New("conflist: at least one pod CIDR is required")
	}

	var sawV4, sawV6 bool
	ranges := make([][]hostLocalRange, 0, len(p.PodCIDRs))
	for _, cidr := range p.PodCIDRs {
		if !cidr.IsValid() {
			return nil, fmt.Errorf("conflist: invalid pod CIDR %q", cidr)
		}
		switch {
		case cidr.Addr().Is4() && !sawV4:
			sawV4 = true
		case cidr.Addr().Is6() && !sawV6:
			sawV6 = true
		default:
			return nil, fmt.Errorf("conflist: more than one pod CIDR for one address family: %s", cidr)
		}
		ranges = append(ranges, []hostLocalRange{{Subnet: cidr.Masked().String()}})
	}

	doc := document{
		CNIVersion: CNIVersion,
		Name:       p.Name,
		Plugins: []any{
			bridgePlugin{
				Type:             "bridge",
				Bridge:           p.Bridge,
				IsDefaultGateway: true,
				HairpinMode:      true,
				MTU:              p.MTU,
				IPAM:             hostLocalIPAM{Type: "host-local", Ranges: ranges},
			},
			portmapPlugin{
				Type:         "portmap",
				Capabilities: map[string]bool{"portMappings": true},
			},
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("conflist: %w", err)
	}
	return append(out, '\n'), nil
}

// The types below mirror the JSON the plugins read. Field order is the order they
// appear in the rendered document.

type document struct {
	CNIVersion string `json:"cniVersion"`
	Name       string `json:"name"`
	Plugins    []any  `json:"plugins"`
}

type bridgePlugin struct {
	Type             string        `json:"type"`
	Bridge           string        `json:"bridge"`
	IsDefaultGateway bool          `json:"isDefaultGateway"`
	HairpinMode      bool          `json:"hairpinMode"`
	MTU              int           `json:"mtu,omitempty"`
	IPAM             hostLocalIPAM `json:"ipam"`
}

type hostLocalIPAM struct {
	Type   string             `json:"type"`
	Ranges [][]hostLocalRange `json:"ranges"`
}

type hostLocalRange struct {
	Subnet string `json:"subnet"`
}

type portmapPlugin struct {
	Type         string          `json:"type"`
	Capabilities map[string]bool `json:"capabilities"`
}
