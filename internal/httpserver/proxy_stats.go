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
	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
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
	NativeCompactionArmed uint64             `json:"native_compaction_armed"`
	RecentClients         []proxyClientStats `json:"recent_clients"`
	Relays                []relayStatus      `json:"relays"`
	Usage                 []proxyModelUsage  `json:"usage"`
}

type proxyModelUsage struct {
	Model    string           `json:"model"`
	Rolling  proxyUsagePeriod `json:"rolling_7d"`
	Lifetime proxyUsagePeriod `json:"lifetime"`
}

type proxyUsagePeriod struct {
	proxytelemetry.Totals
	TokensPerMinute float64 `json:"tokens_per_minute"`
	TokensPerHour   float64 `json:"tokens_per_hour"`
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
	scrubRejected, nativeCompactionArmed            uint64
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

func (s *proxyStats) NativeCompactionArmed() {
	s.Lock()
	s.nativeCompactionArmed++
	s.Unlock()
}

func (s *proxyStats) snapshot() proxySnapshot {
	s.Lock()
	defer s.Unlock()
	result := proxySnapshot{
		Active: s.active, Requests: s.requests, Failovers: s.failovers,
		ScrubbedRequests: s.scrubbed, ScrubbedInputBytes: s.scrubInput, ScrubbedOutputBytes: s.scrubOutput,
		ScrubbedRemovedBytes: s.scrubRemoved, ScrubRejectedRequests: s.scrubRejected, RecentClients: []proxyClientStats{},
		NativeCompactionArmed: s.nativeCompactionArmed,
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
	snapshot.Usage, err = s.proxyUsage(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, snapshot)
}

func (s *Server) proxyUsage(ctx context.Context) ([]proxyModelUsage, error) {
	rows, err := proxytelemetry.ReadTotals(ctx, s.App.Store.DB)
	if err != nil {
		return nil, err
	}
	byModel := map[string]*proxyModelUsage{}
	all := &proxyModelUsage{Model: "all"}
	for _, row := range rows {
		item := byModel[row.Model]
		if item == nil {
			item = &proxyModelUsage{Model: row.Model}
			byModel[row.Model] = item
		}
		target, combined := &item.Rolling.Totals, &all.Rolling.Totals
		if row.Period == proxytelemetry.LifetimePeriod {
			target, combined = &item.Lifetime.Totals, &all.Lifetime.Totals
		}
		*target = row
		addTotals(combined, row)
	}
	result := []proxyModelUsage{*all}
	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		result = append(result, *byModel[model])
	}
	for index := range result {
		result[index].Rolling = withRates(result[index].Rolling.Totals)
		result[index].Lifetime = withRates(result[index].Lifetime.Totals)
	}
	return result, nil
}

func addTotals(target *proxytelemetry.Totals, value proxytelemetry.Totals) {
	target.Responses += value.Responses
	target.ActiveMS += value.ActiveMS
	target.InputTokens += value.InputTokens
	target.CachedInputTokens += value.CachedInputTokens
	target.OutputTokens += value.OutputTokens
	target.ReasoningOutputTokens += value.ReasoningOutputTokens
	target.TotalTokens += value.TotalTokens
}

func withRates(value proxytelemetry.Totals) proxyUsagePeriod {
	result := proxyUsagePeriod{Totals: value}
	if value.ActiveMS > 0 {
		result.TokensPerMinute = float64(value.TotalTokens) * float64(time.Minute/time.Millisecond) / float64(value.ActiveMS)
		result.TokensPerHour = float64(value.TotalTokens) * float64(time.Hour/time.Millisecond) / float64(value.ActiveMS)
	}
	return result
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
