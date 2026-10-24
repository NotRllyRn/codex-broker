package httpserver

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/identity"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/flosch/pongo2/v6"
)

func (s *Server) identitySettings(w http.ResponseWriter, r *http.Request, code string) error {
	c, err := s.settingsContext(r, nil)
	if err != nil {
		return err
	}
	c["identity_code"] = code
	noStore(w)
	return s.renderer.render(w, 200, "settings.html", c)
}

func (s *Server) identityCreate(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_, code, err := identity.Create(r.Context(), s.App.Store, r.Form.Get("name"))
	if err != nil {
		return err
	}
	return s.identitySettings(w, r, code)
}

func (s *Server) identityChange(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	id, action := r.PathValue("id"), r.PathValue("action")
	if action == "rotate" {
		code, err := identity.Rotate(r.Context(), s.App.Store, id)
		if err != nil {
			return err
		}
		return s.identitySettings(w, r, code)
	}
	var result sql.Result
	err := s.App.Store.Write(r.Context(), func(tx store.Executor) error {
		var err error
		switch action {
		case "rename":
			name, err := identity.Name(r.Form.Get("name"))
			if err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM usage_identities WHERE name=? AND identity_id<>?)", name, id).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return core.Conflict("IDENTITY_EXISTS", "An identity with this name already exists")
			}
			result, err = tx.ExecContext(r.Context(), "UPDATE usage_identities SET name=? WHERE identity_id=?", name, id)
		case "delete":
			result, err = tx.ExecContext(r.Context(), "DELETE FROM usage_identities WHERE identity_id=?", id)
		default:
			return core.NewError("NOT_FOUND", "Unknown identity action", 404)
		}
		return err
	})
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return core.NewError("IDENTITY_NOT_FOUND", "Identity not found", 404)
	}
	s.redirect(w, r, "/settings")
	return nil
}

func (s *Server) usageDeviceRevoke(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_, err := s.App.Store.DB.ExecContext(r.Context(), "UPDATE usage_devices SET revoked_at_ms=? WHERE device_id=?", core.NowMS(), r.PathValue("id"))
	if err != nil {
		return err
	}
	s.redirect(w, r, "/settings")
	return nil
}

func (s *Server) identityProxyKey(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	owner, key := r.Form.Get("identity_id"), r.PathValue("id")
	err := s.App.Store.Write(r.Context(), func(tx store.Executor) error {
		var exists bool
		if err := tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM client_api_keys WHERE key_id=? AND revoked_at_ms IS NULL)", key).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return core.NewError("CLIENT_KEY_NOT_FOUND", "Active client key not found", 404)
		}
		if owner != "" {
			if err := tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM usage_identities WHERE identity_id=?)", owner).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return core.NewError("IDENTITY_NOT_FOUND", "Identity not found", 404)
			}
		}
		if _, err := tx.ExecContext(r.Context(), "DELETE FROM identity_proxy_keys WHERE key_id=?", key); err != nil {
			return err
		}
		if owner != "" {
			_, err := tx.ExecContext(r.Context(), "INSERT INTO identity_proxy_keys VALUES(?,?)", key, owner)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.redirect(w, r, "/settings")
	return nil
}

func (s *Server) usageEnroll(w http.ResponseWriter, r *http.Request) error {
	if !s.clientAuth.allow("usage-enroll:" + s.clientAddress(r)) {
		return core.NewError("RATE_LIMITED", "Try enrolling again later", 429)
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Code         string `json:"code"`
		Installation string `json:"installation_id"`
		Name         string `json:"name"`
		Token        string `json:"token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	d, err := identity.Enroll(r.Context(), s.App.Store, body.Code, body.Installation, body.Name, body.Token)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, d)
}

func (s *Server) usageBatch(w http.ResponseWriter, r *http.Request) error {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, err := identity.Authenticate(r.Context(), s.App.Store.DB, token); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var reader io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return core.NewError("INVALID_USAGE", "Invalid compressed batch", 400)
		}
		defer gz.Close()
		reader = gz
	default:
		return core.NewError("UNSUPPORTED_ENCODING", "Use gzip or uncompressed JSON", 415)
	}
	data, err := io.ReadAll(io.LimitReader(reader, (2<<20)+1))
	if err != nil {
		return core.NewError("INVALID_USAGE", "Could not read usage batch", 400)
	}
	if len(data) > 2<<20 {
		return core.NewError("USAGE_TOO_LARGE", "Usage batch exceeds 2 MiB", 413)
	}
	var body struct {
		Version int              `json:"version"`
		Events  []identity.Event `json:"events"`
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil {
		return core.NewError("INVALID_USAGE", "Invalid usage batch", 422)
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return core.NewError("INVALID_USAGE", "Invalid trailing usage data", 422)
	}
	if body.Version != 1 {
		return core.NewError("USAGE_VERSION", "Unsupported usage protocol version", 422)
	}
	n, err := identity.Ingest(r.Context(), s.App.Store, token, body.Events)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]int{"accepted": len(body.Events), "inserted": n})
}

func (s *Server) identityContext(r *http.Request, c pongo2.Context) error {
	items, err := identity.List(r.Context(), s.App.Store.DB)
	if err != nil {
		return err
	}
	devices, err := identity.Devices(r.Context(), s.App.Store.DB)
	if err != nil {
		return err
	}
	rows, err := s.App.Store.DB.QueryContext(r.Context(), "SELECT key_id,identity_id FROM identity_proxy_keys")
	if err != nil {
		return err
	}
	defer rows.Close()
	assignments := map[string]string{}
	for rows.Next() {
		var key, owner string
		if err := rows.Scan(&key, &owner); err != nil {
			return err
		}
		assignments[key] = owner
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c["identities"], c["usage_devices"], c["identity_assignments"] = items, devices, assignments
	return nil
}
