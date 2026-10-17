package broker

import (
	"context"
	"database/sql"
	"sort"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

const (
	weeklyCycleMS          = int64(weekMinutes) * 60 * 1000
	rollingWindowTolerance = int64(90 * 1000)
)

type cycleMember struct {
	id, state               string
	createdAtMS             int64
	shortUsed, weeklyUsed   sql.NullInt64
	weeklyResetS            sql.NullInt64
	weeklyDurationMinutes   sql.NullInt64
	usageReadAtMS           sql.NullInt64
	weeklyStartedAtMS       sql.NullInt64
	lastUsageObservedAtMS   sql.NullInt64
	idleObservationCount    int
	hasActivePulseOperation bool
}

type cycleViewItem struct {
	accountID, state string
	position, size   int
	gapMS            *int64
}

type CycleStatus struct {
	MemberCount, ActiveCount, HeldCount int
	SpacingMS                           int64
	AnchorAtMS, NextPulseAtMS           *int64
}

func cycleInterval(size int) int64 {
	if size < 1 {
		return weeklyCycleMS
	}
	return weeklyCycleMS / int64(size)
}

func weeklyStart(member cycleMember, now int64) (int64, bool) {
	if !member.weeklyResetS.Valid || member.weeklyResetS.Int64*1000 <= now {
		return 0, false
	}
	duration := int64(weekMinutes)
	if member.weeklyDurationMinutes.Valid && member.weeklyDurationMinutes.Int64 > 0 {
		duration = member.weeklyDurationMinutes.Int64
	}
	return member.weeklyResetS.Int64*1000 - duration*60*1000, true
}

func usageCycleEvidence(member cycleMember, now int64) (started int64, active, rolling bool) {
	started, future := weeklyStart(member, now)
	if !future {
		return 0, false, false
	}
	used := member.shortUsed.Valid && member.shortUsed.Int64 > 0 || member.weeklyUsed.Valid && member.weeklyUsed.Int64 > 0
	if member.usageReadAtMS.Valid && !used {
		delta := started - member.usageReadAtMS.Int64
		if delta < 0 {
			delta = -delta
		}
		rolling = delta <= rollingWindowTolerance
	}
	return started, used || member.usageReadAtMS.Valid && !rolling, rolling
}

func loadCycleMembers(ctx context.Context, db store.Executor) ([]cycleMember, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.account_id,a.created_at_ms,u.short_used_percent_raw,u.weekly_used_percent_raw,u.weekly_resets_at_s,u.weekly_duration_minutes,u.complete_read_at_ms,COALESCE(p.cycle_state,''),p.weekly_started_at_ms,p.last_usage_observed_at_ms,COALESCE(p.idle_observation_count,0),EXISTS(SELECT 1 FROM operations o WHERE o.account_id=a.account_id AND o.kind='window.pulse' AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED')) FROM accounts a JOIN account_state s USING(account_id) LEFT JOIN usage_current u USING(account_id) LEFT JOIN window_pulse_state p USING(account_id) WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state IN('STOPPED','CREDENTIAL_IN_USE') ORDER BY a.created_at_ms,a.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []cycleMember
	for rows.Next() {
		var item cycleMember
		if err := rows.Scan(&item.id, &item.createdAtMS, &item.shortUsed, &item.weeklyUsed, &item.weeklyResetS, &item.weeklyDurationMinutes, &item.usageReadAtMS, &item.state, &item.weeklyStartedAtMS, &item.lastUsageObservedAtMS, &item.idleObservationCount, &item.hasActivePulseOperation); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Service) reconcileCycle(ctx context.Context, now int64) error {
	if !s.Config.WindowPulseEnabled {
		return nil
	}
	return s.Store.Write(ctx, func(db store.Executor) error {
		members, err := loadCycleMembers(ctx, db)
		if err != nil {
			return err
		}
		var latestStart sql.NullInt64
		for index := range members {
			member := &members[index]
			started, active, rolling := usageCycleEvidence(*member, now)
			fresh := member.usageReadAtMS.Valid && (!member.lastUsageObservedAtMS.Valid || member.usageReadAtMS.Int64 > member.lastUsageObservedAtMS.Int64)
			if fresh {
				member.lastUsageObservedAtMS = member.usageReadAtMS
				if rolling {
					member.idleObservationCount++
				} else {
					member.idleObservationCount = 0
				}
			}

			state := member.state
			switch {
			case active:
				state = "IN_CYCLE"
				member.weeklyStartedAtMS = sql.NullInt64{Int64: started, Valid: true}
			case state == "RELEASING" && member.hasActivePulseOperation:
			case state == "RELEASING":
				state = "HELD"
			case state == "IN_CYCLE" && member.weeklyStartedAtMS.Valid && member.weeklyStartedAtMS.Int64+weeklyCycleMS <= now:
				state = "HELD"
			case state == "IN_CYCLE" && rolling && member.idleObservationCount >= 2:
				state = "HELD"
			case state != "IN_CYCLE":
				state = "HELD"
			}
			member.state = state
			if state == "IN_CYCLE" && active && (!latestStart.Valid || started > latestStart.Int64) {
				latestStart = sql.NullInt64{Int64: started, Valid: true}
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms,last_usage_observed_at_ms,idle_observation_count) VALUES(?,0,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET cycle_state=excluded.cycle_state,weekly_started_at_ms=excluded.weekly_started_at_ms,last_usage_observed_at_ms=excluded.last_usage_observed_at_ms,idle_observation_count=excluded.idle_observation_count`, member.id, state, nullableInt64(member.weeklyStartedAtMS), nullableInt64(member.lastUsageObservedAtMS), member.idleObservationCount); err != nil {
				return err
			}
		}

		var anchor sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT cadence_anchor_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&anchor); err != nil {
			return err
		}
		if latestStart.Valid && (!anchor.Valid || latestStart.Int64 > anchor.Int64) {
			anchor = latestStart
		}
		if !anchor.Valid && len(members) > 0 {
			anchor = sql.NullInt64{Int64: now - cycleInterval(len(members)), Valid: true}
		}
		_, err = db.ExecContext(ctx, "UPDATE weekly_cycle_state SET cadence_anchor_at_ms=?,member_count=?,state_version=state_version+1 WHERE singleton_id=1 AND (cadence_anchor_at_ms IS NOT ? OR member_count<>?)", nullableInt64(anchor), len(members), nullableInt64(anchor), len(members))
		return err
	})
}

func nullableInt64(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}

func cycleSnapshot(ctx context.Context, db store.Executor) ([]cycleViewItem, CycleStatus, error) {
	members, err := loadCycleMembers(ctx, db)
	if err != nil {
		return nil, CycleStatus{}, err
	}
	status := CycleStatus{MemberCount: len(members), SpacingMS: cycleInterval(len(members))}
	var anchor sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT cadence_anchor_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&anchor); err != nil {
		return nil, status, err
	}
	if anchor.Valid {
		status.AnchorAtMS = &anchor.Int64
		next := anchor.Int64 + status.SpacingMS
		status.NextPulseAtMS = &next
	}

	active := make([]cycleMember, 0, len(members))
	for _, member := range members {
		if member.state == "IN_CYCLE" {
			active = append(active, member)
		} else if member.state == "HELD" || member.state == "RELEASING" {
			status.HeldCount++
		}
	}
	status.ActiveCount = len(active)
	sort.Slice(active, func(i, j int) bool {
		left, right := active[i].weeklyStartedAtMS, active[j].weeklyStartedAtMS
		if left.Valid != right.Valid {
			return left.Valid
		}
		if left.Valid && left.Int64 != right.Int64 {
			return left.Int64 < right.Int64
		}
		return active[i].id < active[j].id
	})
	positions := make(map[string]cycleViewItem, len(active))
	for index, member := range active {
		item := cycleViewItem{accountID: member.id, state: member.state, position: index + 1, size: len(active)}
		if member.weeklyStartedAtMS.Valid && active[len(active)-1].weeklyStartedAtMS.Valid {
			previous := active[len(active)-1].weeklyStartedAtMS.Int64
			if index > 0 {
				previous = active[index-1].weeklyStartedAtMS.Int64
			}
			gap := member.weeklyStartedAtMS.Int64 - previous
			if index == 0 {
				gap += weeklyCycleMS
			}
			item.gapMS = &gap
		}
		positions[member.id] = item
	}
	result := make([]cycleViewItem, 0, len(members))
	for _, member := range members {
		if item, ok := positions[member.id]; ok {
			result = append(result, item)
		} else {
			result = append(result, cycleViewItem{accountID: member.id, state: member.state})
		}
	}
	return result, status, nil
}

func (s *Service) CycleStatus(ctx context.Context) (CycleStatus, error) {
	if !s.Config.WindowPulseEnabled {
		return CycleStatus{}, nil
	}
	_, status, err := cycleSnapshot(ctx, s.Store.DB)
	return status, err
}
