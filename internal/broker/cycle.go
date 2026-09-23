package broker

import (
	"context"
	"database/sql"
	"sort"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

const weeklyCycleMS = int64(weekMinutes) * 60 * 1000

type cycleMember struct {
	id, state               string
	createdAtMS             int64
	weeklyResetS            sql.NullInt64
	weeklyDurationMinutes   sql.NullInt64
	weeklyStartedAtMS       sql.NullInt64
	hasActivePulseOperation bool
}

type cyclePlanItem struct {
	accountID, state string
	position, size   int
	releaseAtMS      int64
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
	started := member.weeklyResetS.Int64*1000 - duration*60*1000
	if started > now {
		started = now
	}
	return started, true
}

func cycleGraceMS(service *Service) int64 {
	seconds := service.Config.WindowPulseRetrySeconds
	if service.Config.UsagePollSeconds*2 > seconds {
		seconds = service.Config.UsagePollSeconds * 2
	}
	if seconds < 60 {
		seconds = 60
	}
	return int64(seconds) * 1000
}

func loadCycleMembers(ctx context.Context, db store.Executor) ([]cycleMember, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.account_id,a.created_at_ms,u.weekly_resets_at_s,u.weekly_duration_minutes,COALESCE(p.cycle_state,''),p.weekly_started_at_ms,EXISTS(SELECT 1 FROM operations o WHERE o.account_id=a.account_id AND o.kind='window.pulse' AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED')) FROM accounts a JOIN account_state s USING(account_id) LEFT JOIN usage_current u USING(account_id) LEFT JOIN window_pulse_state p USING(account_id) WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state IN('STOPPED','CREDENTIAL_IN_USE') ORDER BY a.created_at_ms,a.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []cycleMember
	for rows.Next() {
		var item cycleMember
		if err := rows.Scan(&item.id, &item.createdAtMS, &item.weeklyResetS, &item.weeklyDurationMinutes, &item.state, &item.weeklyStartedAtMS, &item.hasActivePulseOperation); err != nil {
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
		grace := cycleGraceMS(s)
		for index := range members {
			member := &members[index]
			started, active := weeklyStart(*member, now)
			state := "HELD"
			if active {
				state, member.weeklyStartedAtMS = "IN_CYCLE", sql.NullInt64{Int64: started, Valid: true}
			} else if member.state == "RELEASING" && member.hasActivePulseOperation {
				state = "RELEASING"
			} else if member.state == "IN_CYCLE" && member.weeklyStartedAtMS.Valid && now-member.weeklyStartedAtMS.Int64 < grace {
				state = "IN_CYCLE"
			}
			member.state = state
			if member.weeklyStartedAtMS.Valid && (!latestStart.Valid || member.weeklyStartedAtMS.Int64 > latestStart.Int64) {
				latestStart = member.weeklyStartedAtMS
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES(?,0,?,?) ON CONFLICT(account_id) DO UPDATE SET cycle_state=excluded.cycle_state,weekly_started_at_ms=COALESCE(excluded.weekly_started_at_ms,window_pulse_state.weekly_started_at_ms)`, member.id, state, nullableInt64(member.weeklyStartedAtMS)); err != nil {
				return err
			}
		}
		var last sql.NullInt64
		var previousCount int
		if err := db.QueryRowContext(ctx, "SELECT last_release_at_ms,member_count FROM weekly_cycle_state WHERE singleton_id=1").Scan(&last, &previousCount); err != nil {
			return err
		}
		if latestStart.Valid && (previousCount != len(members) || !last.Valid || latestStart.Int64 > last.Int64) {
			last = latestStart
		}
		if !last.Valid && len(members) > 0 {
			last = sql.NullInt64{Int64: now - cycleInterval(len(members)), Valid: true}
		}
		_, err = db.ExecContext(ctx, "UPDATE weekly_cycle_state SET last_release_at_ms=?,member_count=?,state_version=state_version+1 WHERE singleton_id=1 AND (last_release_at_ms IS NOT ? OR member_count<>?)", nullableInt64(last), len(members), nullableInt64(last), len(members))
		return err
	})
}

func nullableInt64(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}

func buildCyclePlan(members []cycleMember, lastRelease sql.NullInt64, now int64) []cyclePlanItem {
	if len(members) == 0 {
		return nil
	}
	type readyMember struct {
		cycleMember
		readyAtMS int64
	}
	ready := make([]readyMember, 0, len(members))
	for _, member := range members {
		available := now
		if member.state == "IN_CYCLE" {
			if member.weeklyResetS.Valid && member.weeklyResetS.Int64*1000 > now {
				available = member.weeklyResetS.Int64 * 1000
			} else if member.weeklyStartedAtMS.Valid && member.weeklyStartedAtMS.Int64+weeklyCycleMS > now {
				available = member.weeklyStartedAtMS.Int64 + weeklyCycleMS
			}
		}
		ready = append(ready, readyMember{member, available})
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].readyAtMS != ready[j].readyAtMS {
			return ready[i].readyAtMS < ready[j].readyAtMS
		}
		if ready[i].createdAtMS != ready[j].createdAtMS {
			return ready[i].createdAtMS < ready[j].createdAtMS
		}
		return ready[i].id < ready[j].id
	})
	cursor := now
	if lastRelease.Valid {
		cursor = lastRelease.Int64 + cycleInterval(len(ready))
	}
	result := make([]cyclePlanItem, 0, len(ready))
	for index, member := range ready {
		release := max(now, cursor, member.readyAtMS)
		result = append(result, cyclePlanItem{member.id, member.state, index + 1, len(ready), release})
		cursor = release + cycleInterval(len(ready))
	}
	return result
}

func (s *Service) cyclePlan(ctx context.Context, now int64) ([]cyclePlanItem, error) {
	if !s.Config.WindowPulseEnabled {
		return nil, nil
	}
	members, err := loadCycleMembers(ctx, s.Store.DB)
	if err != nil {
		return nil, err
	}
	for index := range members {
		if members[index].state == "" {
			members[index].state = "HELD"
			if started, active := weeklyStart(members[index], now); active {
				members[index].state, members[index].weeklyStartedAtMS = "IN_CYCLE", sql.NullInt64{Int64: started, Valid: true}
			}
		}
	}
	var last sql.NullInt64
	if err := s.Store.DB.QueryRowContext(ctx, "SELECT last_release_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&last); err != nil {
		return nil, err
	}
	return buildCyclePlan(members, last, now), nil
}

func observeWeeklyCycle(ctx context.Context, db store.Executor, accountID string, usage Usage, now int64) error {
	if usage.Weekly == nil || usage.Weekly.ResetsAtS == nil || *usage.Weekly.ResetsAtS*1000 <= now {
		return nil
	}
	duration := int64(weekMinutes)
	if usage.Weekly.DurationMinutes != nil && *usage.Weekly.DurationMinutes > 0 {
		duration = *usage.Weekly.DurationMinutes
	}
	started := *usage.Weekly.ResetsAtS*1000 - duration*60*1000
	if started > now {
		started = now
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES(?,0,'IN_CYCLE',?) ON CONFLICT(account_id) DO UPDATE SET cycle_state='IN_CYCLE',weekly_started_at_ms=excluded.weekly_started_at_ms`, accountID, started); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, "UPDATE weekly_cycle_state SET last_release_at_ms=?,state_version=state_version+1 WHERE singleton_id=1 AND (last_release_at_ms IS NULL OR last_release_at_ms<?)", started, started)
	return err
}

func (s *Service) releaseHeld(ctx context.Context, accountID string, now int64) (bool, error) {
	if !s.Config.WindowPulseEnabled {
		return false, nil
	}
	released := false
	err := s.Store.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state) VALUES(?,0,'HELD') ON CONFLICT(account_id) DO NOTHING", accountID); err != nil {
			return err
		}
		result, err := db.ExecContext(ctx, "UPDATE window_pulse_state SET cycle_state='IN_CYCLE',weekly_started_at_ms=?,next_pulse_at_ms=? WHERE account_id=? AND cycle_state='HELD'", now, now, accountID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		released = true
		_, err = db.ExecContext(ctx, "UPDATE weekly_cycle_state SET last_release_at_ms=?,state_version=state_version+1 WHERE singleton_id=1", now)
		return err
	})
	return released, err
}
