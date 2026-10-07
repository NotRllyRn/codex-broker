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
	weeklyUsed              sql.NullInt64
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

func loadCycleMembers(ctx context.Context, db store.Executor) ([]cycleMember, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.account_id,a.created_at_ms,u.weekly_used_percent_raw,u.weekly_resets_at_s,u.weekly_duration_minutes,COALESCE(p.cycle_state,''),p.weekly_started_at_ms,EXISTS(SELECT 1 FROM operations o WHERE o.account_id=a.account_id AND o.kind='window.pulse' AND o.state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED')) FROM accounts a JOIN account_state s USING(account_id) LEFT JOIN usage_current u USING(account_id) LEFT JOIN window_pulse_state p USING(account_id) WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED' AND s.worker_state IN('STOPPED','CREDENTIAL_IN_USE') ORDER BY a.created_at_ms,a.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []cycleMember
	for rows.Next() {
		var item cycleMember
		if err := rows.Scan(&item.id, &item.createdAtMS, &item.weeklyUsed, &item.weeklyResetS, &item.weeklyDurationMinutes, &item.state, &item.weeklyStartedAtMS, &item.hasActivePulseOperation); err != nil {
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
			state := member.state
			switch state {
			case "HELD":
				// Preserve the previous cycle start so overdue accounts keep their order.
			case "RELEASING":
				if !member.hasActivePulseOperation {
					state = "HELD"
				}
			case "IN_CYCLE":
				if member.weeklyStartedAtMS.Valid && member.weeklyStartedAtMS.Int64+weeklyCycleMS <= now {
					state = "HELD"
				} else if !member.weeklyStartedAtMS.Valid {
					started, active := weeklyStart(*member, now)
					if !active || !member.weeklyUsed.Valid || member.weeklyUsed.Int64 <= 0 {
						state = "HELD"
					} else {
						member.weeklyStartedAtMS = sql.NullInt64{Int64: started, Valid: true}
					}
				}
			default:
				started, active := weeklyStart(*member, now)
				if active && member.weeklyUsed.Valid && member.weeklyUsed.Int64 > 0 {
					state, member.weeklyStartedAtMS = "IN_CYCLE", sql.NullInt64{Int64: started, Valid: true}
				} else {
					state = "HELD"
				}
			}
			member.state = state
			if state != "HELD" && member.weeklyStartedAtMS.Valid && (!latestStart.Valid || member.weeklyStartedAtMS.Int64 > latestStart.Int64) {
				latestStart = member.weeklyStartedAtMS
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES(?,0,?,?) ON CONFLICT(account_id) DO UPDATE SET cycle_state=excluded.cycle_state,weekly_started_at_ms=excluded.weekly_started_at_ms`, member.id, state, nullableInt64(member.weeklyStartedAtMS)); err != nil {
				return err
			}
		}
		var last sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT last_release_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&last); err != nil {
			return err
		}
		if latestStart.Valid && (!last.Valid || latestStart.Int64 > last.Int64) {
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
		available := member.createdAtMS
		if member.weeklyStartedAtMS.Valid {
			available = member.weeklyStartedAtMS.Int64 + weeklyCycleMS
		} else if member.state == "IN_CYCLE" && member.weeklyResetS.Valid {
			available = member.weeklyResetS.Int64 * 1000
		}
		if member.state == "HELD" && member.weeklyUsed.Valid && member.weeklyUsed.Int64 > 0 && member.weeklyResetS.Valid {
			available = max(available, member.weeklyResetS.Int64*1000)
		}
		ready = append(ready, readyMember{member, available})
	}
	sort.Slice(ready, func(i, j int) bool {
		if (ready[i].state == "RELEASING") != (ready[j].state == "RELEASING") {
			return ready[i].state == "RELEASING"
		}
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
	return cyclePlan(ctx, s.Store.DB, now)
}

func cyclePlan(ctx context.Context, db store.Executor, now int64) ([]cyclePlanItem, error) {
	members, err := loadCycleMembers(ctx, db)
	if err != nil {
		return nil, err
	}
	for index := range members {
		if members[index].state == "IN_CYCLE" && members[index].weeklyStartedAtMS.Valid && members[index].weeklyStartedAtMS.Int64+weeklyCycleMS <= now {
			members[index].state = "HELD"
		}
		if members[index].state == "" {
			members[index].state = "HELD"
			if started, active := weeklyStart(members[index], now); active && members[index].weeklyUsed.Valid && members[index].weeklyUsed.Int64 > 0 {
				members[index].state, members[index].weeklyStartedAtMS = "IN_CYCLE", sql.NullInt64{Int64: started, Valid: true}
			}
		}
	}
	var last sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT last_release_at_ms FROM weekly_cycle_state WHERE singleton_id=1").Scan(&last); err != nil {
		return nil, err
	}
	return buildCyclePlan(members, last, now), nil
}
