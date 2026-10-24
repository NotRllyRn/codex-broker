package broker

import (
	"context"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

func (s *Service) notifyEnrollment(ctx context.Context, db store.Executor, account Account, email, plan string, raw map[string]any) error {
	rows, err := db.QueryContext(ctx, "SELECT l.name FROM labels l JOIN account_labels al USING(label_id) WHERE al.account_id=? ORDER BY lower(l.name)", account.ID)
	if err != nil {
		return err
	}
	labels := []string{}
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			rows.Close()
			return err
		}
		labels = append(labels, label)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	usage := NormalizeUsage(raw)
	window := func(value *Window) any {
		if value == nil {
			return nil
		}
		return map[string]any{"used_percent": pointerAny(value.UsedPercent), "duration_minutes": pointerAny(value.DurationMinutes), "resets_at_s": pointerAny(value.ResetsAtS)}
	}
	_, err = s.Webhooks.EmitTx(ctx, db, "account.enrolled", "account:"+account.PublicID, map[string]any{
		"account_id": account.PublicID, "account_name": email, "account_email": email, "account_plan": plan,
		"labels": labels, "usage": map[string]any{"short": window(usage.Short), "weekly": window(usage.Weekly)},
		"summary": "A new account completed public enrollment and was added to Codex Broker.",
	}, "", "")
	return err
}
