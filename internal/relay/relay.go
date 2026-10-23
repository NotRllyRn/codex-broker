package relay

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
)

type state struct {
	Snapshot    relayprotocol.Snapshot
	Outbox      []relayprotocol.Event
	Exclusions  map[string]int64
	QuotaBlocks map[string]quotaBlock
}

type quotaBlock struct{ AtMS, RetryAtMS int64 }

type diskEnvelope struct{ Nonce, Ciphertext []byte }

const outboxLimit = 10000

type Relay struct {
	config  config.Config
	client  *http.Client
	mu      sync.RWMutex
	flushMu sync.Mutex
	state   state
	online  bool
}

func Open(settings config.Config) (*Relay, error) {
	if settings.RelayKey == "" || settings.RelayBrokerURL == "" || settings.RelayCACert == "" {
		return nil, errors.New("relay key, broker URL, and broker CA certificate are required")
	}
	certificate, err := os.ReadFile(settings.RelayCACert)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return nil, errors.New("relay broker CA certificate is invalid")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, ForceAttemptHTTP2: true}
	r := &Relay{config: settings, client: &http.Client{Transport: transport}, state: state{Exclusions: map[string]int64{}}}
	if err := r.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("relay cache: %w", err)
	}
	return r, nil
}

func (r *Relay) Start(ctx context.Context) {
	go func() {
		r.sync(ctx)
		ticker := time.NewTicker(time.Duration(r.config.RelaySyncSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sync(ctx)
			}
		}
	}()
}

func (r *Relay) Authenticate(_ *http.Request, token string) (*gateway.Client, error) {
	digest := sha256.Sum256([]byte(token))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.readyLocked(time.Now().UnixMilli()) {
		return nil, core.NewError("RELAY_NOT_READY", "Relay cache is not ready", 503)
	}
	var authenticated *gateway.Client
	for _, key := range r.state.Snapshot.Clients {
		if subtle.ConstantTimeCompare(key.Digest, digest[:]) == 1 {
			authenticated = &gateway.Client{ID: key.ID, Name: key.Name, Prefix: key.Prefix}
		}
	}
	if authenticated == nil {
		return nil, core.NewError("CLIENT_KEY_INVALID", "Client authentication failed", 401)
	}
	return authenticated, nil
}

func (r *Relay) Route(ctx context.Context, clientID string, request gateway.RouteRequest) (*gateway.Lease, *gateway.PoolWait, error) {
	r.mu.RLock()
	online := r.online
	r.mu.RUnlock()
	if online {
		var response relayprotocol.RouteResponse
		callContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := r.call(callContext, http.MethodPost, "/api/private/v1/relay/route", relayprotocol.RouteRequest{ClientID: clientID, Request: request}, &response)
		cancel()
		if err == nil {
			if response.Lease != nil {
				if err := r.updateLease(*response.Lease); err != nil {
					return nil, nil, err
				}
			}
			return response.Lease, response.Wait, nil
		}
		if authoritativeError(err) {
			return nil, nil, err
		}
		r.mu.Lock()
		r.online = false
		r.mu.Unlock()
	}
	return r.localRoute(clientID, request)
}

func (r *Relay) localRoute(clientID string, request gateway.RouteRequest) (*gateway.Lease, *gateway.PoolWait, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UnixMilli()
	if !r.readyLocked(now) {
		return nil, nil, errors.New("relay cache is stale")
	}
	allowed := func(accountID string) bool {
		for _, client := range r.state.Snapshot.Clients {
			if client.ID == clientID {
				return !client.Restricted || slices.Contains(client.AllowedAccountIDs, accountID)
			}
		}
		return false
	}
	if request.FailedAccountID != "" && !allowed(request.FailedAccountID) {
		return nil, nil, core.NewError("FAILED_ACCOUNT_INVALID", "Failed account is not routable", 422)
	}
	if request.FailedAccountID != "" && request.FailureKind != "" {
		expires := now + 5*time.Minute.Milliseconds()
		for _, item := range r.state.Snapshot.Leases {
			if item.AccountID == request.FailedAccountID {
				if reset := leaseReset(item.Lease, now); reset > 0 {
					expires = reset
				}
				break
			}
		}
		if err := r.queueLocked(relayprotocol.Event{ID: core.NewID(), RelayID: r.config.RelayID, Kind: "failure", AccountID: request.FailedAccountID, ClientID: clientID, FailureKind: request.FailureKind, OccurredAtMS: now}, true); err != nil {
			return nil, nil, err
		}
		if request.FailureKind == "quota" {
			r.blockQuotaLocked(request.FailedAccountID, now)
		} else {
			r.state.Exclusions[clientID+"\x00"+request.FailedAccountID] = expires
		}
		if err := r.saveLocked(); err != nil {
			return nil, nil, err
		}
	}
	var active, reserve []relayprotocol.Lease
	for _, item := range r.state.Snapshot.Leases {
		if !allowed(item.AccountID) || item.ExpiresAtMS <= now || exhausted(item.Lease, now) || r.quotaBlockedLocked(item.Lease, now) || r.state.Exclusions[clientID+"\x00"+item.AccountID] > now {
			continue
		}
		if item.Reserve {
			if item.PermitExpiresAtMS > now {
				reserve = append(reserve, item)
			}
			continue
		}
		active = append(active, item)
	}
	selectedReserve := false
	if len(active) == 0 && len(reserve) > 0 {
		active, selectedReserve = reserve, true
	}
	if len(active) == 0 {
		next := int64(math.MaxInt64)
		for _, item := range r.state.Snapshot.Leases {
			if !allowed(item.AccountID) {
				continue
			}
			if expires := r.state.Exclusions[clientID+"\x00"+item.AccountID]; expires > now && expires < next {
				next = expires
			}
			if block := r.state.QuotaBlocks[item.AccountID]; r.quotaBlockedLocked(item.Lease, now) && block.RetryAtMS < next {
				next = block.RetryAtMS
			}
			if reset := leaseReset(item.Lease, now); reset > now && reset < next {
				next = reset
			}
		}
		if next == math.MaxInt64 {
			return nil, nil, errors.New("no cached account is usable")
		}
		seconds := int(math.Ceil(float64(next-now) / 1000))
		return nil, &gateway.PoolWait{NextRetryAtMS: next, RetryAfterSeconds: seconds}, nil
	}
	sort.Slice(active, func(i, j int) bool {
		return leaseLess(active[i], active[j], request.PreferredAccountID, now)
	})
	selected := active[0]
	if selectedReserve {
		selected.Lease.Reserve = true
		selected.Lease.PermitEpoch = selected.PermitEpoch
		selected.Lease.PermitExpiresAtMS = selected.PermitExpiresAtMS
	}
	lease := selected.Lease
	return &lease, nil, nil
}

func (r *Relay) Observe(_ context.Context, value gateway.Observation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.queueLocked(relayprotocol.Event{ID: value.RequestID, RelayID: r.config.RelayID, Kind: "telemetry", OccurredAtMS: value.TerminalEventAtMS, Observation: &value}, false); err != nil {
		return err
	}
	return r.saveLocked()
}

func (r *Relay) ReportFailure(ctx context.Context, clientID, accountID, kind string) error {
	r.mu.Lock()
	now := time.Now().UnixMilli()
	event := relayprotocol.Event{ID: core.NewID(), RelayID: r.config.RelayID, Kind: "failure", AccountID: accountID, ClientID: clientID, FailureKind: kind, OccurredAtMS: now}
	if err := r.queueLocked(event, true); err != nil {
		r.mu.Unlock()
		return err
	}
	if kind == "quota" {
		r.blockQuotaLocked(accountID, now)
	} else {
		r.state.Exclusions[clientID+"\x00"+accountID] = now + 5*time.Minute.Milliseconds()
	}
	err, online := r.saveLocked(), r.online
	r.mu.Unlock()
	if err == nil && online {
		// Do not let queued telemetry delay a shared quota block. The queued
		// copy is reconciled idempotently during the next regular sync.
		if reportErr := r.call(ctx, http.MethodPost, "/api/private/v1/relay/events", relayprotocol.EventBatch{Events: []relayprotocol.Event{event}}, &struct{}{}); reportErr != nil {
			r.mu.Lock()
			r.online = false
			r.mu.Unlock()
		}
	}
	return err
}

func (r *Relay) blockQuotaLocked(accountID string, now int64) {
	retry := int64(math.MaxInt64)
	for _, item := range r.state.Snapshot.Leases {
		if item.AccountID != accountID {
			continue
		}
		for _, reset := range []*int64{item.ShortResetsAtMS, item.WeeklyResetsAtMS} {
			if reset != nil && *reset > now && *reset < retry {
				retry = *reset
			}
		}
	}
	if r.state.QuotaBlocks == nil {
		r.state.QuotaBlocks = map[string]quotaBlock{}
	}
	r.state.QuotaBlocks[accountID] = quotaBlock{AtMS: now, RetryAtMS: retry}
}

func (r *Relay) quotaBlockedLocked(lease gateway.Lease, now int64) bool {
	block, exists := r.state.QuotaBlocks[lease.AccountID]
	return exists && block.RetryAtMS > now && lease.UsageSnapshotAtMS <= block.AtMS
}

func (r *Relay) Activate(_ context.Context, lease gateway.Lease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !lease.Reserve {
		return nil
	}
	now := time.Now().UnixMilli()
	for index := range r.state.Snapshot.Leases {
		item := &r.state.Snapshot.Leases[index]
		if item.AccountID != lease.AccountID || !item.Reserve || item.PermitEpoch != lease.PermitEpoch {
			continue
		}
		if err := r.queueLocked(relayprotocol.Event{ID: core.NewID(), RelayID: r.config.RelayID, Kind: "activation", AccountID: lease.AccountID, PermitEpoch: lease.PermitEpoch, OccurredAtMS: now}, true); err != nil {
			return err
		}
		item.Reserve = false
		item.CycleState = "IN_CYCLE"
		return r.saveLocked()
	}
	return nil
}

func (r *Relay) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.readyLocked(time.Now().UnixMilli())
}

func (r *Relay) readyLocked(now int64) bool {
	snapshot := r.state.Snapshot
	age := now - snapshot.GeneratedAtMS
	if snapshot.ProtocolVersion != relayprotocol.Version || snapshot.RelayID != r.config.RelayID || snapshot.GeneratedAtMS <= 0 || age < -5*time.Minute.Milliseconds() || age > int64(r.config.RelayMaxStaleSeconds)*1000 {
		return false
	}
	for _, item := range snapshot.Leases {
		if item.ExpiresAtMS > now && (!item.Reserve || item.PermitExpiresAtMS > now) {
			return true
		}
	}
	return false
}

func (r *Relay) sync(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := r.flush(ctx); err != nil {
		r.mu.Lock()
		r.online = false
		r.mu.Unlock()
		return
	}
	var snapshot relayprotocol.Snapshot
	path := "/api/private/v1/relay/snapshot?relay_id=" + url.QueryEscape(r.config.RelayID)
	if err := r.call(ctx, http.MethodGet, path, nil, &snapshot); err != nil {
		r.mu.Lock()
		r.online = false
		r.mu.Unlock()
		return
	}
	if snapshot.ProtocolVersion != relayprotocol.Version || snapshot.RelayID != r.config.RelayID {
		r.mu.Lock()
		r.online = false
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	for _, event := range r.state.Outbox {
		if event.Kind != "activation" {
			continue
		}
		for index := range snapshot.Leases {
			item := &snapshot.Leases[index]
			if item.AccountID == event.AccountID && item.PermitEpoch == event.PermitEpoch {
				item.Reserve = false
				item.CycleState = "IN_CYCLE"
			}
		}
	}
	r.state.Snapshot = snapshot
	r.online = true
	for key, expires := range r.state.Exclusions {
		if expires <= time.Now().UnixMilli() {
			delete(r.state.Exclusions, key)
		}
	}
	if err := r.saveLocked(); err != nil {
		r.online = false
		r.mu.Unlock()
		return
	}
	ready := r.readyLocked(time.Now().UnixMilli())
	r.mu.Unlock()
	_ = r.call(ctx, http.MethodPost, "/api/private/v1/relay/heartbeat", relayprotocol.Heartbeat{RelayID: r.config.RelayID, SnapshotID: snapshot.SnapshotID, ProtocolVersion: relayprotocol.Version, CacheGeneratedAtMS: snapshot.GeneratedAtMS, Ready: ready}, &struct{}{})
}

func (r *Relay) flush(ctx context.Context) error {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.RLock()
	events := append([]relayprotocol.Event(nil), r.state.Outbox...)
	r.mu.RUnlock()
	if len(events) == 0 {
		return nil
	}
	if len(events) > 100 {
		events = events[:100]
	}
	if err := r.call(ctx, http.MethodPost, "/api/private/v1/relay/events", relayprotocol.EventBatch{Events: events}, &struct{}{}); err != nil {
		return err
	}
	r.mu.Lock()
	r.state.Outbox = append([]relayprotocol.Event(nil), r.state.Outbox[len(events):]...)
	err := r.saveLocked()
	r.mu.Unlock()
	return err
}

func (r *Relay) call(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.config.RelayBrokerURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.config.RelayKey)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return httpStatusError(response.StatusCode)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output)
}

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("broker returned HTTP %d", e) }

func authoritativeError(err error) bool {
	var value httpStatusError
	if !errors.As(err, &value) {
		return false
	}
	switch value {
	case 400, 401, 403, 409, 422:
		return true
	default:
		return false
	}
}

func (r *Relay) updateLease(lease gateway.Lease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.state.Snapshot.Leases {
		if r.state.Snapshot.Leases[i].AccountID == lease.AccountID {
			r.state.Snapshot.Leases[i].Lease = lease
			return r.saveLocked()
		}
	}
	r.state.Snapshot.Leases = append(r.state.Snapshot.Leases, relayprotocol.Lease{Lease: lease, CycleState: "IN_CYCLE", CreatedAtMS: time.Now().UnixMilli()})
	return r.saveLocked()
}
func (r *Relay) queueLocked(event relayprotocol.Event, critical bool) error {
	if len(r.state.Outbox) >= outboxLimit {
		if !critical {
			return errors.New("relay reconciliation outbox is full")
		}
		index := -1
		for candidate := range r.state.Outbox {
			if r.state.Outbox[candidate].Kind == "telemetry" {
				index = candidate
				break
			}
		}
		if index < 0 {
			return errors.New("relay reconciliation outbox is full")
		}
		r.state.Outbox = append(r.state.Outbox[:index], r.state.Outbox[index+1:]...)
	}
	r.state.Outbox = append(r.state.Outbox, event)
	return nil
}

func exhausted(lease gateway.Lease, now int64) bool {
	return lease.ShortRemainingPercent != nil && *lease.ShortRemainingPercent <= 0 && (lease.ShortResetsAtMS == nil || *lease.ShortResetsAtMS > now) || lease.WeeklyRemainingPercent != nil && *lease.WeeklyRemainingPercent <= 0 && (lease.WeeklyResetsAtMS == nil || *lease.WeeklyResetsAtMS > now)
}
func leaseReset(lease gateway.Lease, now int64) int64 {
	result := int64(0)
	if lease.ShortRemainingPercent != nil && *lease.ShortRemainingPercent <= 0 && lease.ShortResetsAtMS != nil && *lease.ShortResetsAtMS > result {
		result = *lease.ShortResetsAtMS
	}
	if lease.WeeklyRemainingPercent != nil && *lease.WeeklyRemainingPercent <= 0 && lease.WeeklyResetsAtMS != nil && *lease.WeeklyResetsAtMS > result {
		result = *lease.WeeklyResetsAtMS
	}
	if result <= now {
		return 0
	}
	return result
}
func leaseLess(a, b relayprotocol.Lease, preferred string, now int64) bool {
	ar, br := resetRank(a.WeeklyResetsAtMS, now), resetRank(b.WeeklyResetsAtMS, now)
	if ar != br {
		return ar < br
	}
	ar, br = resetRank(a.ShortResetsAtMS, now), resetRank(b.ShortResetsAtMS, now)
	if ar != br {
		return ar < br
	}
	ap, bp := a.AccountID != preferred, b.AccountID != preferred
	if ap != bp {
		return !ap
	}
	if a.CreatedAtMS != b.CreatedAtMS {
		return a.CreatedAtMS < b.CreatedAtMS
	}
	return a.AccountID < b.AccountID
}
func resetRank(value *int64, now int64) int64 {
	if value != nil && *value > now {
		return *value
	}
	return math.MaxInt64
}

func (r *Relay) cacheKey() []byte {
	digest := sha256.Sum256([]byte("codex-broker/relay-cache/v1\x00" + r.config.RelayKey + "\x00" + r.config.RelayID))
	return digest[:]
}
func (r *Relay) load() error {
	content, err := os.ReadFile(r.config.RelayCacheFile)
	if err != nil {
		return err
	}
	var envelope diskEnvelope
	if json.Unmarshal(content, &envelope) != nil {
		return errors.New("invalid encrypted cache")
	}
	block, _ := aes.NewCipher(r.cacheKey())
	gcm, _ := cipher.NewGCM(block)
	plaintext, err := gcm.Open(nil, envelope.Nonce, envelope.Ciphertext, []byte(r.config.RelayID))
	if err != nil {
		return errors.New("encrypted cache authentication failed")
	}
	if json.Unmarshal(plaintext, &r.state) != nil {
		return errors.New("invalid cache payload")
	}
	if r.state.Exclusions == nil {
		r.state.Exclusions = map[string]int64{}
	}
	return nil
}
func (r *Relay) saveLocked() error {
	plaintext, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	block, _ := aes.NewCipher(r.cacheKey())
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	encoded, _ := json.Marshal(diskEnvelope{Nonce: nonce, Ciphertext: gcm.Seal(nil, nonce, plaintext, []byte(r.config.RelayID))})
	dir := filepath.Dir(r.config.RelayCacheFile)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, "relay-cache-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err = temporary.Chmod(0600); err == nil {
		_, err = temporary.Write(encoded)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, r.config.RelayCacheFile); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
