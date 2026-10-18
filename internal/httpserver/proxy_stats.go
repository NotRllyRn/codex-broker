package httpserver

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
)

type proxyClientStats struct {
	Name     string    `json:"name"`
	Prefix   string    `json:"key_prefix"`
	Requests uint64    `json:"requests"`
	Active   int       `json:"active"`
	LastSeen time.Time `json:"last_seen"`
}

type proxySnapshot struct {
	Ready                 bool               `json:"ready"`
	Active                int                `json:"active"`
	Requests              uint64             `json:"requests"`
	Failovers             uint64             `json:"failovers"`
	ScrubbedRequests      uint64             `json:"scrubbed_requests"`
	ScrubbedInputBytes    uint64             `json:"scrubbed_input_bytes"`
	ScrubbedOutputBytes   uint64             `json:"scrubbed_output_bytes"`
	ScrubbedRemovedBytes  uint64             `json:"scrubbed_removed_bytes"`
	ScrubRejectedRequests uint64             `json:"scrub_rejected_requests"`
	RecentClients         []proxyClientStats `json:"recent_clients"`
	Relays                []relayStatus      `json:"relays"`
}

type relayStatus struct {
	ID               string     `json:"id"`
	State            string     `json:"state"`
	Connected        bool       `json:"connected"`
	Ready            bool       `json:"ready"`
	ProtocolVersion  int        `json:"protocol_version"`
	CacheGeneratedAt *time.Time `json:"cache_generated_at"`
	LastSeen         time.Time  `json:"last_seen"`
}

type proxyStats struct {
	sync.Mutex
	active                                          int
	requests, failovers                             uint64
	scrubbed, scrubInput, scrubOutput, scrubRemoved uint64
	scrubRejected                                   uint64
	clients                                         map[string]*proxyClientStats
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

func (s *proxyStats) Begin(key gateway.Client) func() {
	return s.begin(auth.ClientKey{ID: key.ID, Name: key.Name, Prefix: key.Prefix})
}

func (s *proxyStats) failover() { s.Lock(); s.failovers++; s.Unlock() }
func (s *proxyStats) Failover() { s.failover() }

func (s *proxyStats) scrub(before, after int) {
	s.Lock()
	s.scrubbed++
	s.scrubInput += uint64(before)
	s.scrubOutput += uint64(after)
	s.scrubRemoved += uint64(max(0, before-after))
	s.Unlock()
}
func (s *proxyStats) Scrub(before, after int) { s.scrub(before, after) }

func (s *proxyStats) rejectScrub() { s.Lock(); s.scrubRejected++; s.Unlock() }
func (s *proxyStats) RejectScrub() { s.rejectScrub() }

func (s *proxyStats) snapshot() proxySnapshot {
	s.Lock()
	defer s.Unlock()
	result := proxySnapshot{
		Active: s.active, Requests: s.requests, Failovers: s.failovers,
		ScrubbedRequests: s.scrubbed, ScrubbedInputBytes: s.scrubInput, ScrubbedOutputBytes: s.scrubOutput,
		ScrubbedRemovedBytes: s.scrubRemoved, ScrubRejectedRequests: s.scrubRejected, RecentClients: []proxyClientStats{},
	}
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
	snapshot := s.proxyStats.snapshot()
	snapshot.Ready = s.App.Ready
	var err error
	snapshot.Relays, err = s.relayStatuses(r.Context(), time.Now())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, snapshot)
}

func (s *Server) relayStatuses(ctx context.Context, now time.Time) ([]relayStatus, error) {
	rows, err := s.App.Store.DB.QueryContext(ctx, `SELECT relay_id,protocol_version,cache_generated_at_ms,ready,last_seen_at_ms FROM relay_heartbeats ORDER BY last_seen_at_ms DESC,relay_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	statuses := []relayStatus{}
	cutoff := now.Add(-3 * time.Duration(max(5, s.App.Config.RelaySyncSeconds)) * time.Second).UnixMilli()
	for rows.Next() {
		var status relayStatus
		var cache sql.NullInt64
		var ready, lastSeen int64
		if err := rows.Scan(&status.ID, &status.ProtocolVersion, &cache, &ready, &lastSeen); err != nil {
			return nil, err
		}
		status.LastSeen = time.UnixMilli(lastSeen).UTC()
		status.Connected = lastSeen >= cutoff
		status.Ready = status.Connected && ready == 1
		status.State = "offline"
		if status.Connected {
			status.State = "connected"
		}
		if status.Ready {
			status.State = "ready"
		}
		if cache.Valid {
			generated := time.UnixMilli(cache.Int64).UTC()
			status.CacheGeneratedAt = &generated
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}
