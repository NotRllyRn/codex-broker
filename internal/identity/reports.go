package identity

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/usagereport"
)

type Snapshot struct {
	Revision      int64           `json:"revision"`
	Report        json.RawMessage `json:"report"`
	AllowDecrease bool            `json:"allow_decrease,omitempty"`
}

type ReportBatch struct {
	Version        int        `json:"version"`
	CCUsageVersion string     `json:"ccusage_version"`
	Snapshots      []Snapshot `json:"snapshots"`
}

func IngestReports(ctx context.Context, db *store.Store, token string, batch ReportBatch) (int, error) {
	if batch.Version != 2 || batch.CCUsageVersion != usagereport.Version {
		return 0, invalid("Unsupported ccusage/protocol version; update the sync client")
	}
	if len(batch.Snapshots) > 100 {
		return 0, invalid("Upload at most 100 daily reports per batch")
	}
	parsed := make([]usagereport.Row, len(batch.Snapshots))
	for i, snapshot := range batch.Snapshots {
		raw, row, err := usagereport.Canonical(snapshot.Report)
		if err != nil {
			return 0, invalid(err.Error())
		}
		if snapshot.Revision <= 0 || row.Period > time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly) {
			return 0, invalid("Invalid report revision/date")
		}
		batch.Snapshots[i].Report = raw
		parsed[i] = row
	}
	updated := 0
	err := db.Write(ctx, func(tx store.Executor) error {
		device, err := Authenticate(ctx, tx, token)
		if err != nil {
			return err
		}
		for i, snapshot := range batch.Snapshots {
			day := parsed[i].Period
			var revision int64
			var payload []byte
			err := tx.QueryRowContext(ctx, "SELECT revision,payload FROM usage_reports WHERE device_id=? AND day=?", device.ID, day).Scan(&revision, &payload)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if revision > snapshot.Revision {
					return core.Conflict("USAGE_REVISION_STALE", "Broker has a newer report; restore the current client journal instead of an older backup")
				}
				if revision == snapshot.Revision {
					if !bytes.Equal(payload, snapshot.Report) {
						return core.Conflict("USAGE_REVISION_CONFLICT", "A report revision already has different data")
					}
					continue
				}
				_, old, err := usagereport.Canonical(payload)
				if err != nil {
					return err
				}
				if !snapshot.AllowDecrease && usagereport.Decreases(old, parsed[i]) {
					return core.Conflict("USAGE_DECREASE", "Report tokens decreased; restore logs or explicitly allow a correction")
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO usage_reports VALUES(?,?,?,?,?,?) ON CONFLICT(device_id,day) DO UPDATE SET revision=excluded.revision,ccusage_version=excluded.ccusage_version,payload=excluded.payload,updated_at_ms=excluded.updated_at_ms`, device.ID, day, snapshot.Revision, batch.CCUsageVersion, string(snapshot.Report), core.NowMS())
			if err != nil {
				return err
			}
			updated++
		}
		_, err = tx.ExecContext(ctx, "UPDATE usage_devices SET last_sync_at_ms=? WHERE device_id=?", core.NowMS(), device.ID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return updated, nil
}
