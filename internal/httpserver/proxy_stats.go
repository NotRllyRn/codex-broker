package httpserver

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
)

type proxyClientStats struct {
	Name     string    `json:"name"`
	Prefix   string    `json:"key_prefix"`
	Requests uint64    `json:"requests"`
	Active   int       `json:"active"`
	LastSeen time.Time `json:"last_seen"`
}

type proxySnapshot struct {
	Active        int                `json:"active"`
	Requests      uint64             `json:"requests"`
	Failovers     uint64             `json:"failovers"`
	RecentClients []proxyClientStats `json:"recent_clients"`
}

type proxyStats struct {
	sync.Mutex
	active              int
	requests, failovers uint64
	clients             map[string]*proxyClientStats
}

func (s *proxyStats) begin(key auth.ClientKey) func() {
	s.Lock()
	if s.clients == nil {
		s.clients = map[string]*proxyClientStats{}
	}
	client := s.clients[key.ID]
	if client == nil {
		client = &proxyClientStats{Name: key.Name, Prefix: key.Prefix}
		s.clients[key.ID] = client
	}
	s.active++
	s.requests++
	client.Active++
	client.Requests++
	client.LastSeen = time.Now()
	s.Unlock()
	return func() {
		s.Lock()
		s.active--
		client.Active--
		s.Unlock()
	}
}

func (s *proxyStats) failover() { s.Lock(); s.failovers++; s.Unlock() }

func (s *proxyStats) snapshot() proxySnapshot {
	s.Lock()
	defer s.Unlock()
	result := proxySnapshot{Active: s.active, Requests: s.requests, Failovers: s.failovers, RecentClients: []proxyClientStats{}}
	for _, client := range s.clients {
		if client.Active > 0 || time.Since(client.LastSeen) < 5*time.Minute {
			result.RecentClients = append(result.RecentClients, *client)
		}
	}
	sort.Slice(result.RecentClients, func(i, j int) bool { return result.RecentClients[i].LastSeen.After(result.RecentClients[j].LastSeen) })
	return result
}

func (s *Server) internalProxy(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	return writeJSON(w, 200, s.proxyStats.snapshot())
}
