package auth

import (
	"context"
	"database/sql"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

type LabelScope struct{ ID, Name, Mode string }

// AccountScopeSQL expects accounts aliased as a and a named scope_key_id parameter.
// No allow rules means unrestricted, except for explicit blocks.
const AccountScopeSQL = `(
 NOT EXISTS (SELECT 1 FROM client_key_labels WHERE key_id=:scope_key_id AND mode='allow')
 OR EXISTS (SELECT 1 FROM client_key_labels kl JOIN account_labels al USING(label_id) WHERE kl.key_id=:scope_key_id AND kl.mode='allow' AND al.account_id=a.account_id)
) AND NOT EXISTS (SELECT 1 FROM client_key_labels kl JOIN account_labels al USING(label_id) WHERE kl.key_id=:scope_key_id AND kl.mode='block' AND al.account_id=a.account_id)`

func (c *ClientKeys) Labels(ctx context.Context) ([]LabelScope, error) {
	rows, err := c.store.DB.QueryContext(ctx, "SELECT label_id,name FROM labels ORDER BY lower(name)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var labels []LabelScope
	for rows.Next() {
		var label LabelScope
		if err := rows.Scan(&label.ID, &label.Name); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func saveScopes(ctx context.Context, db store.Executor, keyID string, scopes []LabelScope) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM client_key_labels WHERE key_id=?", keyID); err != nil {
		return err
	}
	for _, scope := range scopes {
		var exists bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM labels WHERE label_id=?)", scope.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists || (scope.Mode != "allow" && scope.Mode != "block") {
			return core.NewError("CLIENT_KEY_SCOPE_INVALID", "Choose existing labels and allow or block rules", 422)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO client_key_labels VALUES(?,?,?)", keyID, scope.ID, scope.Mode); err != nil {
			return err
		}
	}
	return nil
}

func (c *ClientKeys) SetScopes(ctx context.Context, keyID string, scopes []LabelScope) error {
	return c.store.Write(ctx, func(db store.Executor) error {
		var active bool
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_api_keys WHERE key_id=? AND revoked_at_ms IS NULL)", keyID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return core.NewError("CLIENT_KEY_NOT_FOUND", "Active client key not found", 404)
		}
		return saveScopes(ctx, db, keyID, scopes)
	})
}

// AllowedAccountIDs materializes the same policy for offline relays.
func (c *ClientKeys) AllowedAccountIDs(ctx context.Context, keyID string) ([]string, error) {
	rows, err := c.store.DB.QueryContext(ctx, "SELECT a.public_token FROM accounts a WHERE a.deleted_at_ms IS NULL AND "+AccountScopeSQL, sql.Named("scope_key_id", keyID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
