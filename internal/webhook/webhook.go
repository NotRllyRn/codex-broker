package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

var retrySeconds = []int64{60, 300, 1800, 7200, 21600, 43200, 86400, 86400}
var specialNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001:10::/28"),
}

type Dispatcher struct {
	store  *store.Store
	vault  *vault.Vault
	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	work   sync.WaitGroup
}

func New(database *store.Store, secrets *vault.Vault) *Dispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{store: database, vault: secrets, ctx: ctx, cancel: cancel, stop: make(chan struct{})}
}
func (d *Dispatcher) Start() {
	d.work.Add(1)
	go func() { defer d.work.Done(); d.loop() }()
}
func (d *Dispatcher) Close() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	d.cancel()
	d.work.Wait()
}

func validateURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("webhook URL must be an absolute HTTPS URL without credentials")
	}
	if address, err := netip.ParseAddr(parsed.Hostname()); err == nil && !publicAddress(address) {
		return errors.New("webhook URL host must be public")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("webhook URL host must be public")
	}
	return nil
}

func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsUnspecified() {
		return false
	}
	for _, network := range specialNetworks {
		if network.Contains(address) {
			return false
		}
	}
	return true
}

func (d *Dispatcher) Create(ctx context.Context, name, target, secret, kind string) (string, error) {
	if err := validateURL(target); err != nil {
		return "", err
	}
	kind = strings.ToLower(kind)
	if kind != "generic" && kind != "slack" && kind != "discord" {
		return "", errors.New("webhook destination kind is unsupported")
	}
	id, now := core.NewID(), core.NowMS()
	name = truncateText(strings.Join(strings.Fields(name), " "), 80)
	sealedURL, err := d.vault.SealText("webhook:"+id+":url", target)
	if err != nil {
		return "", err
	}
	var sealedSecret []byte
	if secret != "" {
		sealedSecret, err = d.vault.SealText("webhook:"+id+":secret", secret)
		if err != nil {
			return "", err
		}
	}
	urlHash := sha256.Sum256(sealedURL)
	var secretNonce any
	var secretValue any
	if sealedSecret != nil {
		digest := sha256.Sum256(sealedSecret)
		secretNonce = digest[:12]
		secretValue = sealedSecret
	}
	err = d.store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "INSERT INTO webhook_destinations VALUES(?,?,?,?,?,?,?,?,?,?)", id, name, kind, 1, urlHash[:12], sealedURL, secretNonce, secretValue, now, now)
		return err
	})
	return id, err
}

func (d *Dispatcher) Destinations(ctx context.Context) ([]map[string]any, error) {
	rows, err := d.store.DB.QueryContext(ctx, "SELECT destination_id,display_name,kind,enabled,created_at_ms FROM webhook_destinations ORDER BY lower(display_name)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []map[string]any
	for rows.Next() {
		var id, name, kind string
		var enabled int
		var created int64
		if err := rows.Scan(&id, &name, &kind, &enabled, &created); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"destination_id": id, "display_name": name, "kind": kind, "enabled": enabled == 1, "created_at_ms": created})
	}
	return result, rows.Err()
}
func (d *Dispatcher) SetEnabled(ctx context.Context, id string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	return d.store.Write(ctx, func(db store.Executor) error {
		result, err := db.ExecContext(ctx, "UPDATE webhook_destinations SET enabled=?,updated_at_ms=? WHERE destination_id=?", value, core.NowMS(), id)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return errors.New("webhook destination not found")
		}
		return nil
	})
}
func (d *Dispatcher) Delete(ctx context.Context, id string) error {
	return d.store.Write(ctx, func(db store.Executor) error {
		result, err := db.ExecContext(ctx, "DELETE FROM webhook_destinations WHERE destination_id=?", id)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return errors.New("webhook destination not found")
		}
		return nil
	})
}

func (d *Dispatcher) Emit(ctx context.Context, eventType, subject string, data map[string]any, incidentID, destinationID string) (string, error) {
	id, now := core.NewID(), core.NowMS()
	title := strings.ToUpper(strings.ReplaceAll(eventType, ".", " "))
	if eventType == "codex-broker.test" {
		title = "WEBHOOK TEST"
	}
	event := map[string]any{"schema": "codex-broker.webhook/v1", "event_id": id, "event_type": eventType, "subject": subject, "occurred_at_ms": now, "occurred_at": time.UnixMilli(now).UTC().Format("2006-01-02T15:04:05.000000+00:00"), "notification": map[string]any{"source": "CODEX_BROKER", "code": eventCode(eventType), "title": title}, "data": logbook.Redact(data)}
	generic, _ := json.Marshal(event)
	err := d.store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "INSERT INTO webhook_events VALUES(?,?,?,?,?,?,?)", id, eventType, subject, now, generic, nullString(incidentID), now); err != nil {
			return err
		}
		query := "SELECT destination_id,kind FROM webhook_destinations WHERE enabled=1"
		args := []any{}
		if destinationID != "" {
			query = "SELECT destination_id,kind FROM webhook_destinations WHERE destination_id=?"
			args = []any{destinationID}
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		found := false
		for rows.Next() {
			found = true
			var destination, kind string
			if err := rows.Scan(&destination, &kind); err != nil {
				return err
			}
			body := providerBody(kind, event)
			if _, err := db.ExecContext(ctx, "INSERT INTO webhook_deliveries VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", core.NewID(), id, destination, "PENDING", 0, now, body, "application/json", nil, nil, nil, nil, nil, now, nil); err != nil {
				return err
			}
		}
		if destinationID != "" && !found {
			return errors.New("webhook destination not found")
		}
		return rows.Err()
	})
	return id, err
}
func (d *Dispatcher) Test(ctx context.Context, id string) (string, error) {
	return d.Emit(ctx, "codex-broker.test", "destination:"+id, map[string]any{"destination_id": id, "message": "Codex Broker successfully created a test notification.", "delivery_status": "DELIVERED", "severity": "INFO", "recommended_action": "No action required. This confirms the destination accepts Codex Broker webhooks."}, "", id)
}
func eventCode(value string) string {
	switch value {
	case "incident.opened":
		return "CB-101"
	case "incident.updated":
		return "CB-102"
	case "incident.resolved":
		return "CB-103"
	case "codex-broker.test":
		return "CB-900"
	}
	return "CB-999"
}
func providerBody(kind string, event map[string]any) []byte {
	message := notificationText(event)
	if kind == "slack" {
		escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(message)
		parts := strings.SplitN(escaped, "\n", 2)
		body := ""
		if len(parts) == 2 {
			body = strings.TrimSpace(parts[1])
		}
		event = map[string]any{"text": truncateText(escaped, 3000), "blocks": []any{map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncateText(parts[0], 150)}}, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncateText(body, 3000)}}}}
	} else if kind == "discord" {
		parts := strings.SplitN(message, "\n", 2)
		body := ""
		if len(parts) == 2 {
			body = strings.TrimSpace(parts[1])
		}
		notification, _ := event["notification"].(map[string]any)
		event = map[string]any{"content": parts[0], "allowed_mentions": map[string]any{"parse": []any{}}, "embeds": []any{map[string]any{"title": textValue(notification["title"], 600), "description": truncateText(body, 4000), "footer": map[string]any{"text": "Codex Broker event " + textValue(event["event_id"], 64)}}}}
	}
	body, _ := json.Marshal(event)
	return body
}

func notificationText(event map[string]any) string {
	data, _ := event["data"].(map[string]any)
	notification, _ := event["notification"].(map[string]any)
	account := textValue(data["account_name"], 600)
	if account == "" {
		account = textValue(event["subject"], 600)
	}
	email := textValue(data["account_email"], 600)
	if email != "" {
		account += " <" + email + ">"
	}
	lines := []string{"CODEX BROKER · " + textValue(notification["code"], 600), textValue(notification["title"], 600), "", "Account: " + account}
	status, severity := textValue(data["incident_status"], 600), textValue(data["severity"], 600)
	if status == "" {
		status = textValue(data["delivery_status"], 600)
	}
	if status != "" || severity != "" {
		if status == "" {
			status = "UNKNOWN"
		}
		if severity == "" {
			severity = "INFO"
		}
		lines = append(lines, "Status: "+status+" · Severity: "+severity)
	}
	summary := textValue(data["summary"], 600)
	if summary == "" {
		summary = textValue(data["message"], 600)
	}
	if summary == "" {
		summary = textValue(event["subject"], 600)
	}
	lines = append(lines, "What happened: "+summary)
	if cause := textValue(data["cause_summary"], 600); cause != "" && cause != summary {
		lines = append(lines, "Cause: "+textValue(data["cause_code"], 600)+" — "+cause)
	}
	for _, item := range []struct{ prefix, key string }{{"Why it matters: ", "reason"}, {"How to fix: ", "recommended_action"}} {
		if value := textValue(data[item.key], 600); value != "" {
			lines = append(lines, item.prefix+value)
		}
	}
	if data["occurrence_count"] != nil {
		lines = append(lines, "Occurrences: "+textValue(data["occurrence_count"], 600))
	}
	if value := textValue(data["incident_id"], 64); value != "" {
		lines = append(lines, "Incident ID: "+value)
	}
	return strings.Join(append(lines, "Event ID: "+textValue(event["event_id"], 64), "Occurred: "+textValue(event["occurred_at"], 600)), "\n")
}

func textValue(value any, limit int) string {
	if value == nil {
		return ""
	}
	return truncateText(strings.Join(strings.Fields(fmt.Sprint(value)), " "), limit)
}
func truncateText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

type delivery struct {
	id, event, destination              string
	attempt                             int
	body, encryptedURL, encryptedSecret []byte
	contentType, lease                  string
}

func (d *Dispatcher) claim(ctx context.Context) (*delivery, error) {
	now := core.NowMS()
	lease := core.NewID()
	var item delivery
	err := d.store.Write(ctx, func(db store.Executor) error {
		row := db.QueryRowContext(ctx, `SELECT d.delivery_id,d.event_id,d.destination_id,d.attempt_count,d.immutable_body,d.content_type,w.encrypted_url,w.encrypted_signing_secret FROM webhook_deliveries d JOIN webhook_destinations w USING(destination_id) WHERE d.state IN('PENDING','RETRY_SCHEDULED') AND d.next_attempt_at_ms<=? AND w.enabled=1 ORDER BY d.next_attempt_at_ms LIMIT 1`, now)
		var secret sql.Null[[]byte]
		if err := row.Scan(&item.id, &item.event, &item.destination, &item.attempt, &item.body, &item.contentType, &item.encryptedURL, &secret); err != nil {
			return err
		}
		if secret.Valid {
			item.encryptedSecret = secret.V
		}
		result, err := db.ExecContext(ctx, "UPDATE webhook_deliveries SET state='LEASED',lease_token=?,lease_expires_at_ms=? WHERE delivery_id=? AND state IN('PENDING','RETRY_SCHEDULED')", lease, now+30000, item.id)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return sql.ErrNoRows
		}
		item.lease = lease
		return nil
	})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &item, err
}

func (d *Dispatcher) loop() {
	for {
		select {
		case <-d.stop:
			return
		default:
		}
		item, err := d.claim(d.ctx)
		if err != nil || item == nil {
			select {
			case <-d.stop:
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		d.deliver(item)
	}
}

func (d *Dispatcher) deliver(item *delivery) {
	target, err := d.vault.OpenText(item.encryptedURL)
	if err != nil {
		d.finish(item, 0, "WEBHOOK_DECRYPT_FAILED", "")
		return
	}
	secret := ""
	if item.encryptedSecret != nil {
		secret, _ = d.vault.OpenText(item.encryptedSecret)
	}
	parsed, _ := url.Parse(target)
	addresses, err := net.DefaultResolver.LookupNetIP(d.ctx, "ip", parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		d.finish(item, 0, "WEBHOOK_RESOLUTION_FAILED", "")
		return
	}
	var selected netip.Addr
	for _, address := range addresses {
		if !publicAddress(address) {
			d.finish(item, 0, "WEBHOOK_DESTINATION_BLOCKED", "")
			return
		}
		if !selected.IsValid() {
			selected = address
		}
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	dial := net.JoinHostPort(selected.String(), port)
	transport := &http.Transport{Proxy: nil, DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, network, dial, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
		return connection, err
	}}
	client := http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequestWithContext(d.ctx, http.MethodPost, target, strings.NewReader(string(item.body)))
	request.Header.Set("Content-Type", item.contentType)
	request.Header.Set("User-Agent", "codex-broker/0.1")
	request.Header.Set("X-Codex-Broker-Event-ID", item.event)
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(item.body)
		request.Header.Set("X-Codex-Broker-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	response, err := client.Do(request)
	if err != nil {
		d.finish(item, 0, "WEBHOOK_NETWORK_ERROR", "")
		return
	}
	defer response.Body.Close()
	excerpt, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	code := ""
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code = "WEBHOOK_HTTP_ERROR"
	}
	sanitized, _ := logbook.Redact(string(excerpt)).(string)
	delay := int64(0)
	if response.StatusCode == http.StatusTooManyRequests {
		delay = retryAfter(response.Header.Get("Retry-After"))
		if delay < 1 || delay > 86400 {
			delay = 0
		}
	}
	d.finish(item, response.StatusCode, code, sanitized, delay)
}
func (d *Dispatcher) finish(item *delivery, status int, code, excerpt string, providerDelay ...int64) {
	attempt := item.attempt + 1
	succeeded := code == "" && status >= 200 && status < 300
	state := "RETRY_SCHEDULED"
	if succeeded {
		state = "SUCCEEDED"
	} else if attempt >= len(retrySeconds) {
		state = "FAILED"
	}
	now := core.NowMS()
	next := now
	if state == "RETRY_SCHEDULED" {
		delay := retrySeconds[attempt-1]
		if len(providerDelay) > 0 && providerDelay[0] > 0 {
			delay = providerDelay[0]
		}
		next += delay * 1000
	}
	var statusValue any
	if status > 0 {
		statusValue = status
	}
	var completed any
	if state != "RETRY_SCHEDULED" {
		completed = now
	}
	_ = d.store.Write(context.Background(), func(db store.Executor) error {
		_, err := db.ExecContext(context.Background(), "UPDATE webhook_deliveries SET state=?,attempt_count=?,next_attempt_at_ms=?,lease_token=NULL,lease_expires_at_ms=NULL,last_status_code=?,last_error_code=?,last_response_excerpt=?,completed_at_ms=? WHERE delivery_id=? AND lease_token=?", state, attempt, next, statusValue, nullString(code), excerpt, completed, item.id, item.lease)
		return err
	})
}
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func retryAfter(value string) int64 { number, _ := strconv.ParseInt(value, 10, 64); return number }

var _ = retryAfter
