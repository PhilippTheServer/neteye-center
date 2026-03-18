// Package models defines the shared data structures used throughout neteye-center.
// These types are also the wire protocol between agent→center and center→frontend.
package models

import "time"

// ─── Agent → Center protocol ──────────────────────────────────────────────────

// AgentMessage is the top-level envelope sent by an agent over WebSocket.
// The Type field determines which payload field is populated.
type AgentMessage struct {
	Type string `json:"type"` // "register" | "update"

	// register
	Register *AgentRegistration `json:"register,omitempty"`

	// update
	Update *AgentUpdate `json:"update,omitempty"`
}

// AgentRegistration is sent once when an agent connects.
type AgentRegistration struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
}

// AgentUpdate is a full state snapshot sent every collection interval.
type AgentUpdate struct {
	Timestamp  time.Time       `json:"timestamp"`
	Interfaces []InterfaceInfo `json:"interfaces"`
	Routes     []Route         `json:"routes"`
}

// InterfaceInfo describes a single network interface and its current metrics.
type InterfaceInfo struct {
	Name      string          `json:"name"`
	MAC       string          `json:"mac"`
	State     string          `json:"state"` // "up" | "down"
	MTU       int             `json:"mtu"`
	SpeedMbps int64           `json:"speed_mbps"`
	Addresses []AddressInfo   `json:"addresses"`
	Metrics   InterfaceMetrics `json:"metrics"`
}

// AddressInfo holds a single IP address in CIDR notation.
type AddressInfo struct {
	Address string `json:"address"` // e.g. "192.168.1.10/24"
	Family  string `json:"family"`  // "ipv4" | "ipv6"
}

// InterfaceMetrics holds counters for a single interface at a point in time.
// All values are absolute (monotonic counters from the kernel).
type InterfaceMetrics struct {
	BytesRecv   uint64 `json:"bytes_recv"`
	BytesSent   uint64 `json:"bytes_sent"`
	PacketsRecv uint64 `json:"packets_recv"`
	PacketsSent uint64 `json:"packets_sent"`
	ErrorsIn    uint64 `json:"errors_in"`
	ErrorsOut   uint64 `json:"errors_out"`
	DropsIn     uint64 `json:"drops_in"`
	DropsOut    uint64 `json:"drops_out"`
}

// Route represents a single entry in the kernel routing table.
type Route struct {
	Destination   string `json:"destination"`    // CIDR, e.g. "0.0.0.0/0"
	Gateway       string `json:"gateway"`        // IP or "" for on-link
	InterfaceName string `json:"interface_name"` // e.g. "eth0"
	Metric        int    `json:"metric"`
	Flags         string `json:"flags"` // e.g. "UG"
}

// ─── Center → Frontend protocol ───────────────────────────────────────────────

// FrontendMessage is the top-level envelope pushed to frontend WebSocket clients.
type FrontendMessage struct {
	Type string `json:"type"`
	// "topology"      → Topology
	// "device_update" → DeviceUpdate
	// "device_offline"→ DeviceOffline
	// "metrics"       → MetricsUpdate

	Topology      *TopologySnapshot `json:"topology,omitempty"`
	DeviceUpdate  *DeviceInfo       `json:"device_update,omitempty"`
	DeviceOffline *DeviceOfflineMsg `json:"device_offline,omitempty"`
	Metrics       *MetricsUpdate    `json:"metrics,omitempty"`
}

// TopologySnapshot is sent on initial frontend connection and after major changes.
type TopologySnapshot struct {
	Devices   []DeviceInfo `json:"devices"`
	Timestamp time.Time    `json:"timestamp"`
}

// DeviceInfo is the full view of a device as seen by the frontend.
type DeviceInfo struct {
	ID         string          `json:"id"`
	Hostname   string          `json:"hostname"`
	OS         string          `json:"os"`
	Arch       string          `json:"arch"`
	Status     string          `json:"status"` // "online" | "offline"
	LastSeen   time.Time       `json:"last_seen"`
	FirstSeen  time.Time       `json:"first_seen"`
	Interfaces []InterfaceInfo `json:"interfaces"`
	Routes     []Route         `json:"routes"`
}

// DeviceOfflineMsg notifies the frontend that a device stopped reporting.
type DeviceOfflineMsg struct {
	DeviceID string    `json:"device_id"`
	Hostname string    `json:"hostname"`
	LastSeen time.Time `json:"last_seen"`
}

// MetricsUpdate carries per-interface rate metrics (bytes/s, packets/s)
// computed from successive raw counter samples.
type MetricsUpdate struct {
	DeviceID      string    `json:"device_id"`
	InterfaceName string    `json:"interface_name"`
	Timestamp     time.Time `json:"timestamp"`
	// Rates (per second)
	BytesRecvRate   float64 `json:"bytes_recv_rate"`
	BytesSentRate   float64 `json:"bytes_sent_rate"`
	PacketsRecvRate float64 `json:"packets_recv_rate"`
	PacketsSentRate float64 `json:"packets_sent_rate"`
	ErrorsInRate    float64 `json:"errors_in_rate"`
	ErrorsOutRate   float64 `json:"errors_out_rate"`
	DropsInRate     float64 `json:"drops_in_rate"`
	DropsOutRate    float64 `json:"drops_out_rate"`
}

// ─── REST API response types ───────────────────────────────────────────────────

// NetworkGroup groups devices that share a common subnet, derived from
// CIDR prefix matching across all interface addresses.
type NetworkGroup struct {
	CIDR    string       `json:"cidr"`
	Members []DeviceInfo `json:"members"`
}

// RouteAnalysis is returned by the /api/route endpoint.
type RouteAnalysis struct {
	Source      string     `json:"source"`
	Destination string     `json:"destination"`
	Found       bool       `json:"found"`
	Hops        []RouteHop `json:"hops"`
	Networks    []string   `json:"networks"` // network CIDRs traversed
}

// RouteHop is a single step in a RouteAnalysis path.
type RouteHop struct {
	DeviceID  string `json:"device_id"`
	Hostname  string `json:"hostname"`
	ViaCIDR   string `json:"via_cidr"`   // shared subnet used to reach next hop
	IngressIP string `json:"ingress_ip"` // IP on the incoming side
	EgressIP  string `json:"egress_ip"`  // IP on the outgoing side
}
