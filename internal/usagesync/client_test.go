package usagesync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func daily(output int) []byte {
	return []byte(fmt.Sprintf(`{"daily":[{"period":"2026-09-01","agent":"all","inputTokens":10,"outputTokens":%d,"cacheReadTokens":20,"cacheCreationTokens":0,"totalTokens":%d,"totalCost":0.01,"agents":[{"agent":"generic-tool","outputTokens":%d}]}]}`, output, 30+output, output))
}
func TestOfflineJournalAndLostAcknowledgements(t *testing.T) {
	directory := t.TempDir()
	config := Config{URL: "https://unused", Code: "cbi_private", Token: "cbu_token", Installation: "installation", Name: "Laptop"}
	if err := atomicJSON(filepath.Join(directory, "config.json"), config); err != nil {
		t.Fatal(err)
	}
	data := daily(5)
	offline := true
	lost := true
	enrollments := []map[string]string{}
	stored := map[string]Entry{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline {
			w.WriteHeader(503)
			return
		}
		if strings.HasSuffix(r.URL.Path, "devices") {
			body := map[string]string{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			enrollments = append(enrollments, body)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "device"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer cbu_token" {
			t.Error("wrong token")
		}
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer reader.Close()
		var body struct {
			Version   int     `json:"version"`
			Snapshots []Entry `json:"snapshots"`
		}
		if err := json.NewDecoder(reader).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Version != 2 {
			t.Error("wrong protocol")
		}
		for _, entry := range body.Snapshots {
			var row struct {
				Period string `json:"period"`
			}
			_ = json.Unmarshal(entry.Report, &row)
			stored[row.Period] = entry
		}
		if len(body.Snapshots) > 0 && lost {
			lost = false
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(body.Snapshots)})
	}))
	defer server.Close()
	config.URL = server.URL
	client := &Client{Directory: directory, Config: config, HTTP: server.Client(), Output: io.Discard, Collect: func(context.Context) ([]byte, error) { return data, nil }}
	if err := client.Sync(t.Context(), false); err == nil {
		t.Fatal("offline success")
	}
	journal := Journal{}
	if err := readJSON(filepath.Join(directory, "reports.json"), &journal); err != nil || journal["2026-09-01"].Acked {
		t.Fatal("offline history not retained", err)
	}
	offline = false
	if err := client.Sync(t.Context(), false); err == nil {
		t.Fatal("lost ack success")
	}
	if err := client.Sync(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored["2026-09-01"].Revision != 1 {
		t.Fatal("retry inflated usage", stored)
	}
	if len(enrollments) != 1 {
		t.Fatal("enrollment repeated after success")
	}
	var saved Config
	if err := readJSON(filepath.Join(directory, "config.json"), &saved); err != nil || saved.Code != "" {
		t.Fatal("code retained", err)
	}
	data = daily(8)
	if err := client.Sync(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if stored["2026-09-01"].Revision != 2 {
		t.Fatal("change not uploaded")
	}
	data = daily(2)
	if err := client.Sync(t.Context(), false); err == nil {
		t.Fatal("log loss accepted")
	}
	if stored["2026-09-01"].Revision != 2 {
		t.Fatal("log loss changed server")
	}
	if err := client.Sync(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if stored["2026-09-01"].Revision != 3 || !stored["2026-09-01"].AllowDecrease {
		t.Fatal("explicit correction failed")
	}
	data = []byte(`{"daily":[]}`)
	if err := client.Sync(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatal("missing history deleted")
	}
}
func TestLockAndAtomicState(t *testing.T) {
	directory := t.TempDir()
	unlock, err := lock(directory)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lock(directory); err == nil {
		second()
		t.Fatal("concurrent sync allowed")
	}
	unlock()
	again, err := lock(directory)
	if err != nil {
		t.Fatal(err)
	}
	again()
	path := filepath.Join(directory, "state.json")
	for _, value := range []string{"old", "new"} {
		if err := atomicJSON(path, map[string]string{"value": value}); err != nil {
			t.Fatal(err)
		}
	}
	var state map[string]string
	if err := readJSON(path, &state); err != nil || state["value"] != "new" {
		t.Fatal(err)
	}
}
func TestTLSAndRedirects(t *testing.T) {
	if _, err := transport(Config{URL: "http://example.com"}); err == nil {
		t.Fatal("HTTP accepted")
	}
	client, err := transport(Config{URL: "https://example.com/broker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(nil, nil); err == nil {
		t.Fatal("redirect accepted")
	}
}
func TestNativeArchiveIntegrity(t *testing.T) {
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gz)
	content := []byte("native-executable")
	if err := archive.WriteHeader(&tar.Header{Name: "package/bin/ccusage", Mode: 0700, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	_, _ = archive.Write(content)
	_ = archive.Close()
	_ = gz.Close()
	sum := sha512.Sum512(buffer.Bytes())
	integrity := base64.StdEncoding.EncodeToString(sum[:])
	data, err := unpackNative(buffer.Bytes(), integrity, "linux")
	if err != nil || !bytes.Equal(data, content) {
		t.Fatal(err)
	}
	if _, err := unpackNative(buffer.Bytes(), "wrong", "linux"); err == nil {
		t.Fatal("unverified package accepted")
	}
	if _, err := Native(t.Context(), "plan9", "amd64"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
}
func TestSchedulesAndQuoting(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "windows"} {
		directory := filepath.Join(t.TempDir(), "Some space & percent % dollar $")
		data := scheduleContent(directory, platform, "S-1-5-21-example")
		if !strings.Contains(data, "sync") || !strings.Contains(data, "--state-dir") || strings.Contains(data, "cbu_") {
			t.Fatal(platform, "invalid schedule")
		}
		if platform == "windows" {
			for _, text := range []string{"InteractiveToken", "StartWhenAvailable", "PT1H", "LogonTrigger"} {
				if !strings.Contains(data, text) {
					t.Fatal("missing", text)
				}
			}
		}
		if platform != "linux" {
			decoder := xml.NewDecoder(strings.NewReader(strings.Replace(data, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)))
			for {
				_, err := decoder.Token()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(platform, err)
				}
			}
		}
	}
	if got := windowsQuote(`C:\some dir\`); got != `"C:\some dir\\"` {
		t.Fatal(got)
	}
}
func TestSourceEnvironmentExcludesSecrets(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/logs")
	t.Setenv("CODEX_API_KEY", "secret")
	t.Setenv("UNRELATED_PATH", "secret")
	result := strings.Join(sourceEnvironment(), "\n")
	if !strings.Contains(result, "CODEX_HOME=/custom/logs") || strings.Contains(result, "secret") {
		t.Fatal(result)
	}
}
func TestCorruptJournalDoesNotUpload(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "reports.json"), []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	client := Client{Directory: directory}
	if err := client.Sync(t.Context(), false); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}

func TestFailedJournalWriteNeverUploadsNewReports(t *testing.T) {
	directory := t.TempDir()
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": 0})
	}))
	defer server.Close()
	client := Client{Directory: directory, Config: Config{URL: server.URL, Token: "cbu_token"}, HTTP: server.Client(), Output: io.Discard, Collect: func(context.Context) ([]byte, error) {
		path := filepath.Join(directory, "reports.json")
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, err
		}
		return daily(5), nil
	}}
	if err := client.Sync(t.Context(), false); err == nil {
		t.Fatal("journal write failure ignored")
	}
	if requests != 1 {
		t.Fatal("new usage uploaded without durable local state", requests)
	}
}

func TestCollectorUpgradePreservesState(t *testing.T) {
	binary := os.Getenv("CCUSAGE_TEST_BINARY")
	if binary == "" {
		t.Skip("set CCUSAGE_TEST_BINARY to test real collector upgrades")
	}
	collector, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	directory, source := t.TempDir(), t.TempDir()
	files := map[string][]byte{
		"config.json":   []byte(`{"device_id":"existing-device","token":"existing-token"}`),
		"reports.json":  []byte(`{"2026-09-01":{"revision":3,"acked":false}}`),
		"ccusage.json":  []byte(`{"timezone":"UTC"}`),
		"broker-ca.pem": []byte("existing private CA"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(directory, executable("ccusage"))
	old := []byte("obsolete collector")
	if err := writeExecutable(target, old); err != nil {
		t.Fatal(err)
	}
	fetch := func(context.Context, string, string) ([]byte, error) { return []byte("broken download"), nil }
	if err := ensureCollector(t.Context(), directory, source, fetch); err == nil {
		t.Fatal("accepted broken replacement")
	}
	if data, _ := os.ReadFile(target); !bytes.Equal(data, old) {
		t.Fatal("failed upgrade replaced old collector")
	}
	calls := 0
	fetch = func(context.Context, string, string) ([]byte, error) { calls++; return collector, nil }
	for range 2 {
		if err := ensureCollector(t.Context(), directory, source, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("downloaded %d times, want one upgrade and no redundant download", calls)
	}
	var config Config
	if err := readJSON(filepath.Join(directory, "config.json"), &config); err != nil {
		t.Fatal(err)
	}
	if err := install(t.Context(), directory, &config, nil, "", "", "", "", false, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		if got, err := os.ReadFile(filepath.Join(directory, name)); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("upgrade changed %s: %v", name, err)
		}
	}
}

func TestUpdateConnectionPreservesEnrollment(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		t.Run(fmt.Sprint(enrolled), func(t *testing.T) {
			directory := t.TempDir()
			config := Config{URL: "https://wrong.example", Code: "pending-code", Installation: "existing-installation", Token: "existing-token", Environment: []string{"CODEX_HOME=/example"}}
			if enrolled {
				config.Code, config.Device = "", "existing-device"
			}
			want := config
			want.URL = "https://correct.example/broker"
			journal := []byte(`{"2026-09-01":{"revision":3,"acked":false}}`)
			if err := os.WriteFile(filepath.Join(directory, "reports.json"), journal, 0600); err != nil {
				t.Fatal(err)
			}
			if err := updateConnection(directory, &config, want.URL+"/", ""); err != nil {
				t.Fatal(err)
			}
			var saved Config
			if err := readJSON(filepath.Join(directory, "config.json"), &saved); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(saved)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatal("connection update changed enrollment or environment")
			}
			for _, invalid := range []string{"http://insecure.example", "https://example.com?secret=value", "not a URL"} {
				if err := updateConnection(directory, &config, invalid, ""); err == nil {
					t.Fatal("accepted invalid URL", invalid)
				}
			}
			if err := updateConnection(directory, &config, "https://another.example", filepath.Join(directory, "missing.pem")); err == nil {
				t.Fatal("accepted missing CA")
			}
			data, _ := os.ReadFile(filepath.Join(directory, "config.json"))
			if !bytes.Equal(data, wantJSON) || config.URL != want.URL {
				t.Fatal("failed override changed saved connection")
			}
			data, _ = os.ReadFile(filepath.Join(directory, "reports.json"))
			if !bytes.Equal(data, journal) {
				t.Fatal("connection update changed queued reports")
			}
		})
	}
}

func TestUpdateConnectionCopiesValidatedCA(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	directory := t.TempDir()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(ca, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	config := Config{URL: server.URL, Device: "existing-device"}
	if err := updateConnection(directory, &config, "", ca); err != nil {
		t.Fatal(err)
	}
	if config.CA == ca || filepath.Dir(config.CA) != directory {
		t.Fatal("CA was not copied into private state")
	}
	client, err := transport(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal("copied CA did not establish trust:", err)
	}
	response.Body.Close()
}
