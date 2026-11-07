package httpserver

import (
	"context"
	"net/http"

	"github.com/NotRllyRn/codex-broker/extensions/settings"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
)

func (s *Server) proxyExtensionSettings(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store")
	if err := s.requireSession(r); err != nil {
		return err
	}
	manager := s.proxyGateway().Extensions
	id := r.PathValue("id")
	secrets := s.App.Vault
	if !s.App.VaultConfigured {
		secrets = nil
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, settings.MaxBytes*2)
		if err := s.requireForm(r); err != nil {
			return err
		}
		var input struct {
			Values   settings.Values `json:"values"`
			Revision string          `json:"revision"`
			Reset    bool            `json:"reset"`
		}
		if err := decodeJSON(r, &input); err != nil {
			return core.NewError("INVALID_EXTENSION_SETTINGS", "Invalid or oversized settings JSON", 400)
		}
		if err := manager.SaveSettings(r.Context(), s.App.Store, secrets, id, input.Revision, input.Values, input.Reset); err != nil {
			return err
		}
		if err := manager.RefreshSettings(r.Context(), func(ctx context.Context) (proxyextension.Configuration, error) {
			return proxyextension.Configurations(ctx, s.App.Store, secrets)
		}); err != nil {
			return err
		}
	}
	view, err := manager.SettingsView(r.Context(), s.App.Store, secrets, id)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, view)
}
