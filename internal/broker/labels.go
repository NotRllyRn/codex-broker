package broker

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

func (s *Service) CreateLabel(ctx context.Context, name string) error {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || utf8.RuneCountInString(name) > 40 || strings.Contains(name, ",") {
		return core.NewError("ACCOUNT_LABELS_INVALID", "Use a label of 1-40 characters without commas", 422)
	}
	return s.Store.Write(ctx, func(db store.Executor) error {
		_, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO labels VALUES(?,?,?)", core.NewID(), name, core.NowMS())
		return err
	})
}

func (s *Service) DeleteLabel(ctx context.Context, id string) error {
	err := s.Store.Write(ctx, func(db store.Executor) error {
		var used int
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_key_labels WHERE label_id=?)", id).Scan(&used); err != nil {
			return err
		}
		if used != 0 {
			return core.NewError("LABEL_IN_USE", "Remove this label from client key access rules before deleting it", 409)
		}
		_, err := db.ExecContext(ctx, "DELETE FROM labels WHERE label_id=?", id)
		return err
	})
	if err == nil {
		s.Events.Publish("account.updated", map[string]any{"label_deleted": id})
	}
	return err
}
