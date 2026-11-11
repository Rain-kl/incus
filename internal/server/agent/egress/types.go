// Package egress manages the host's optional transparent Xray gateway.
// The host forwards packets through Incus ACLs BEFORE the namespace intercepts them.
package egress

import "encoding/json"

// Policy selects the host egress mode and proxy health policy.
type Policy struct {
	Mode                 string   `json:"mode"`
	Strategy             string   `json:"strategy"`
	ProxyNodeIDs         []int    `json:"proxyNodeIds"`
	InternalCIDRs        []string `json:"internalCidrs"`
	ProbeURL             string   `json:"probeUrl"`
	ProbeIntervalSeconds int      `json:"probeIntervalSeconds"`
}

// Rule describes a validated host firewall rule.
type Rule struct {
	Direction       string `json:"direction"`
	Action          string `json:"action"`
	Protocol        string `json:"protocol"`
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	SourcePort      string `json:"sourcePort"`
	DestinationPort string `json:"destinationPort"`
	Description     string `json:"description"`
	Enabled         bool   `json:"enabled"`
}

// Firewall holds managed ACL policy and rule defaults.
type Firewall struct {
	Enabled       bool   `json:"enabled"`
	IngressAction string `json:"ingressAction"`
	EgressAction  string `json:"egressAction"`
	Rules         []Rule `json:"rules"`
}

// Node holds a proxy identity and its outbound configuration.
type Node struct {
	ID       int             `json:"id"`
	Outbound json.RawMessage `json:"outbound"`
}

// ProbeRequest specifies the bounded set of proxy nodes to test.
type ProbeRequest struct {
	ProbeURL string `json:"probeUrl"`
	Nodes    []Node `json:"nodes"`
}

// Instruction is a versioned desired network policy.
type Instruction struct {
	SchemaVersion  int             `json:"schemaVersion,omitempty"`
	Groups         []EgressGroup   `json:"groups,omitempty"`
	Bindings       []EgressBinding `json:"bindings,omitempty"`
	GroupDirectDNS string          `json:"-"`
	GroupBridgeDNS []string        `json:"-"`
	GroupInternal  []string        `json:"-"`
	Revision       int             `json:"revision"`
	Egress         Policy          `json:"egress"`
	Firewall       Firewall        `json:"firewall"`
	Nodes          []Node          `json:"nodes"`
}

// Status reports applied network revision and health without proxy credentials.
type Status struct {
	Groups          []EgressGroupStatus   `json:"groups,omitempty"`
	Bindings        []EgressBindingStatus `json:"bindings,omitempty"`
	AppliedRevision int                   `json:"appliedRevision"`
	State           string                `json:"state"`
	Error           string                `json:"error"`
	ErrorStage      string                `json:"errorStage,omitempty"`
	ErrorDetail     string                `json:"errorDetail,omitempty"`
	XrayAvailable   bool                  `json:"xrayAvailable"`
	ActiveNodeID    int                   `json:"activeNodeId,omitempty"`
	HealthyNodeIDs  []int                 `json:"healthyNodeIds"`
	ManagedNetworks []string              `json:"managedNetworks"`
	RoutedFallback  bool                  `json:"routedFallback"`
}

// ProbeResult reports connectivity and sanitized errors for one proxy node.
type ProbeResult struct {
	NodeID      int    `json:"nodeId"`
	Success     bool   `json:"success"`
	LatencyMs   int64  `json:"latencyMs"`
	Error       string `json:"error"`
	ErrorDetail string `json:"errorDetail,omitempty"`
}
