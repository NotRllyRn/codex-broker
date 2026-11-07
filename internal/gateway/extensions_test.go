package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func TestNativeIdentityNeverUsesCacheOrConnection(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("Session-ID", "session-affinity")
	r.Header.Set("X-Forwarded-For", "192.0.2.1")
	identity := extensionIdentity(r, []byte(`{"prompt_cache_key":"cache"}`), "client", "request")
	if identity.ThreadID != "" {
		t.Fatal(identity)
	}
	r.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"native","turn_id":"turn"}`)
	identity = extensionIdentity(r, nil, "client", "request")
	if identity.ThreadID != "native" || identity.TurnID != "turn" {
		t.Fatal(identity)
	}
	r.Header.Set("Thread-ID", "preferred")
	if got := extensionIdentity(r, nil, "client", "request"); got.ThreadID != "preferred" {
		t.Fatal(got)
	}
}
func TestExtensionBinaryBodyAndHeaders(t *testing.T) {
	def, err := proxyextension.Compile("binary", `export default p=>p.on('body',e=>({bodyBase64:e.bodyBase64,headers:{...e.headers,'Content-Length':['900'],'Authorization':['private'],'X-Example':['ok']}}))`)
	if err != nil {
		t.Fatal(err)
	}
	s, err := proxyextension.New(def).Start(t.Context(), proxyextension.Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte{0xff, 0x00, 0xfe}
	r := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}
	w := httptest.NewRecorder()
	err = extensionResponse(s, w, r, func(dst http.ResponseWriter) error { responsesproxy.CopyResponse(dst, r); return nil })
	if err != nil || !bytes.Equal(w.Body.Bytes(), body) || w.Header().Get("Content-Length") != "" || w.Header().Get("Authorization") != "" || w.Header().Get("X-Example") != "ok" {
		t.Fatal(w, err)
	}
}
func TestInvalidSyntheticResponse(t *testing.T) {
	for _, value := range []any{map[string]any{"status": 101}, map[string]any{"status": 200, "headers": map[string][]string{"X-Test": {"bad\r\nheader"}}}, map[string]any{"status": 200, "bodyBase64": "!"}} {
		if r, err := localResponse(value); err == nil {
			t.Fatal(r)
		}
	}
}

func TestInvalidExtensionConfigurationIsUnready(t *testing.T) {
	t.Setenv("WINDOWKEEPER_PROXY_EXTENSIONS_DIR", "/nonexistent-proxy-extension-directory")
	engine := New(nil, nil)
	if engine.ExtensionError == nil || engine.Ready() {
		t.Fatal("invalid extension configuration reported ready")
	}
}

func TestExtensionHeadersRejectTransportInvalidValues(t *testing.T) {
	for _, header := range []http.Header{{"Invalid Name": {"value"}}, {"π": {"value"}}, {"X-Example": {"value\x00"}}, {"X-Example": {"value\x7f"}}} {
		if _, err := extensionHeaders(header); err == nil {
			t.Fatal("accepted invalid header", header)
		}
	}
	if _, err := extensionHeaders(http.Header{"X-Example": {"valid\tvalue"}}); err != nil {
		t.Fatal(err)
	}
}
