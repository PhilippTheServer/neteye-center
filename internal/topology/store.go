// Package topology maintains an in-memory view of the network topology
// derived from live agent reports. It provides fast reads for the frontend
// without hitting PostgreSQL on every WebSocket push.
package topology

import (
	"net"
	"sync"
	"time"

	"github.com/neteye/center/internal/models"
)

// Store is a thread-safe in-memory store of DeviceInfo records.
type Store struct {
	mu      sync.RWMutex
	devices map[string]*models.DeviceInfo // deviceID → DeviceInfo
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{devices: make(map[string]*models.DeviceInfo)}
}

// Upsert inserts or replaces the record for a device.
func (s *Store) Upsert(d models.DeviceInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := d
	s.devices[d.ID] = &cp
}

// Get returns a copy of the DeviceInfo for the given ID, or nil.
func (s *Store) Get(id string) *models.DeviceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[id]
	if !ok {
		return nil
	}
	cp := *d
	return &cp
}

// SetOffline marks a device as offline.
func (s *Store) SetOffline(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.devices[id]; ok {
		d.Status = "offline"
		d.LastSeen = time.Now()
	}
}

// Snapshot returns a TopologySnapshot with all known devices.
func (s *Store) Snapshot() models.TopologySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	devices := make([]models.DeviceInfo, 0, len(s.devices))
	for _, d := range s.devices {
		devices = append(devices, *d)
	}
	return models.TopologySnapshot{
		Devices:   devices,
		Timestamp: time.Now(),
	}
}

// LoadFromDB seeds the in-memory store from a slice of DeviceInfo records
// loaded from PostgreSQL at startup.
func (s *Store) LoadFromDB(devices []models.DeviceInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range devices {
		cp := d
		s.devices[d.ID] = &cp
	}
}

// ─── Route analysis ──────────────────────────────────────────────────────────

// AnalyzeRoute performs a BFS across the shared-subnet graph to find the
// shortest path from srcID to dstID.
//
// Two devices are considered "adjacent" when they share at least one /prefix
// that contains both their addresses. This mirrors how real IP routing works:
// hosts on the same subnet can reach each other directly.
func (s *Store) AnalyzeRoute(srcID, dstID string) *models.RouteAnalysis {
	s.mu.RLock()
	devices := make(map[string]*models.DeviceInfo, len(s.devices))
	for k, v := range s.devices {
		cp := *v
		devices[k] = &cp
	}
	s.mu.RUnlock()

	src, ok := devices[srcID]
	if !ok {
		return &models.RouteAnalysis{Found: false}
	}
	dst, ok := devices[dstID]
	if !ok {
		return &models.RouteAnalysis{Found: false}
	}

	// Build adjacency: deviceID → list of (neighborID, sharedCIDR, srcIP, dstIP)
	type edge struct {
		neighbor string
		cidr     string
		srcIP    string
		dstIP    string
	}
	adj := make(map[string][]edge, len(devices))
	for _, d := range devices {
		adj[d.ID] = nil
	}

	// Index: CIDR → list of (deviceID, ip)
	type netMember struct {
		deviceID string
		ip       string
	}
	netIndex := make(map[string][]netMember)
	for _, d := range devices {
		for _, iface := range d.Interfaces {
			for _, addr := range iface.Addresses {
				_, ipNet, err := net.ParseCIDR(addr.Address)
				if err != nil {
					continue
				}
				cidr := ipNet.String()
				ip, _, _ := net.ParseCIDR(addr.Address)
				netIndex[cidr] = append(netIndex[cidr], netMember{d.ID, ip.String()})
			}
		}
	}
	for cidr, members := range netIndex {
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				a, b := members[i], members[j]
				adj[a.deviceID] = append(adj[a.deviceID], edge{b.deviceID, cidr, a.ip, b.ip})
				adj[b.deviceID] = append(adj[b.deviceID], edge{a.deviceID, cidr, b.ip, a.ip})
			}
		}
	}

	// BFS
	type state struct {
		id   string
		path []models.RouteHop
	}
	visited := map[string]bool{srcID: true}
	queue := []state{{
		id: srcID,
		path: []models.RouteHop{{
			DeviceID: srcID,
			Hostname: src.Hostname,
		}},
	}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, e := range adj[cur.id] {
			if visited[e.neighbor] {
				continue
			}
			visited[e.neighbor] = true
			dev := devices[e.neighbor]
			hop := models.RouteHop{
				DeviceID:  e.neighbor,
				Hostname:  dev.Hostname,
				ViaCIDR:   e.cidr,
				IngressIP: e.srcIP,
				EgressIP:  e.dstIP,
			}
			newPath := append(append([]models.RouteHop{}, cur.path...), hop)

			if e.neighbor == dstID {
				nets := uniqueNets(newPath)
				return &models.RouteAnalysis{
					Source:      src.Hostname,
					Destination: dst.Hostname,
					Found:       true,
					Hops:        newPath,
					Networks:    nets,
				}
			}
			queue = append(queue, state{e.neighbor, newPath})
		}
	}

	return &models.RouteAnalysis{
		Source:      src.Hostname,
		Destination: dst.Hostname,
		Found:       false,
	}
}

func uniqueNets(hops []models.RouteHop) []string {
	seen := make(map[string]bool)
	var out []string
	for _, h := range hops {
		if h.ViaCIDR != "" && !seen[h.ViaCIDR] {
			seen[h.ViaCIDR] = true
			out = append(out, h.ViaCIDR)
		}
	}
	return out
}
