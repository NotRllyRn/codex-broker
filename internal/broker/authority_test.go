package broker

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

var fakeMarker = flag.String("fake-marker", "", "fake app-server refresh marker")

func TestBrokerFakeAppServer(t *testing.T) {
	if *fakeMarker == "" {
		return
	}
	decoder, encoder := json.NewDecoder(bufio.NewReader(os.Stdin)), json.NewEncoder(os.Stdout)
	for {
		var request map[string]any
		if decoder.Decode(&request) != nil {
			os.Exit(0)
		}
		id, ok := request["id"]
		if !ok {
			continue
		}
		result := map[string]any{}
		switch request["method"] {
		case "initialize":
			result["server"] = "fake"
		case "account/read":
			file, _ := os.OpenFile(*fakeMarker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			_, _ = file.WriteString("refresh\n")
			_ = file.Close()
			content, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": testJWT(time.Now().Add(time.Hour).Unix()), "refresh_token": "never-return"}})
			_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), content, 0o600)
			if strings.Contains(filepath.Base(*fakeMarker), "delete-auth") {
				_ = os.Remove(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"))
			}
			result["account"] = map[string]any{"type": "chatgpt", "email": "owner@example.com"}
		}
		_ = encoder.Encode(map[string]any{"id": id, "result": result})
	}
}

func testJWT(expiry int64) string {
	claims, _ := json.Marshal(map[string]any{"exp": expiry, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "upstream"}})
	return "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func TestConcurrentNearExpiryLeasesRefreshOnce(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	instance, _ := database.InstanceID(ctx)
	secrets, _ := vault.New(make([]byte, 32), instance)
	marker, script := filepath.Join(root, "refreshes"), filepath.Join(root, "codex")
	command := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestBrokerFakeAppServer -fake-marker=%s -- \"$@\"\n", strconv.Quote(os.Args[0]), strconv.Quote(marker))
	if err := os.WriteFile(script, []byte(command), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := config.Config{RuntimeDir: filepath.Join(root, "run"), CodexExecutable: script, CodexVersion: "test", ProcessStartConcurrency: 2, UsageRefreshConcurrency: 4, WindowPulseConcurrency: 2, AuthConcurrency: 2}
	service := NewService(database, settings, secrets, codex.NewRuntimeManager(settings, secrets), events.New(100, 10))
	authHome := filepath.Join(root, "auth")
	if err := os.MkdirAll(authHome, 0o700); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": testJWT(time.Now().Add(time.Minute).Unix()), "refresh_token": "never-return"}})
	if err := os.WriteFile(filepath.Join(authHome, "auth.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Capture(authHome, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := database.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "INSERT INTO accounts VALUES('internal','public','Primary','chatgpt','CHATGPT_DEVICE_CODE','CHATGPT_DEVICE_CODE',NULL,1,'ACTIVE',?,?,NULL)", now, now); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO account_state VALUES('internal','VERIFIED','STOPPED','HEALTHY','FRESH','owner@example.com',NULL,?,?,NULL,NULL,NULL,1,?)", now, now, now); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, "INSERT INTO usage_current(account_id,short_used_percent_raw,weekly_used_percent_raw) VALUES('internal',10,40)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.installBundle(ctx, "internal", "ACTIVE", payload); err != nil {
		t.Fatal(err)
	}
	router := Router{Store: database, Service: service, PaddingSeconds: 10}
	start := make(chan struct{})
	errors := make(chan error, 50)
	var group sync.WaitGroup
	for index := 0; index < 50; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			lease, _, err := router.Route(ctx, "key", RouteRequest{SessionID: "session", TurnID: strconv.Itoa(index)})
			if err == nil && (lease == nil || lease.ChatGPTAccountID != "upstream") {
				err = fmt.Errorf("invalid lease: %#v", lease)
			}
			errors <- err
		}(index)
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	refreshes, err := os.ReadFile(marker)
	if err != nil || string(refreshes) != "refresh\n" {
		t.Fatalf("refresh marker = %q, %v", refreshes, err)
	}
	var active, retired int
	_ = database.DB.QueryRowContext(ctx, "SELECT count(*) FROM credential_bundles WHERE account_id='internal' AND state='ACTIVE'").Scan(&active)
	_ = database.DB.QueryRowContext(ctx, "SELECT count(*) FROM credential_bundles WHERE account_id='internal' AND state='RETIRED'").Scan(&retired)
	if active != 1 || retired != 1 {
		t.Fatalf("credential generations: active=%d retired=%d", active, retired)
	}
}

func TestCheckpointFailureQuarantinesCredential(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	instance, _ := database.InstanceID(ctx)
	secrets, _ := vault.New(make([]byte, 32), instance)
	marker, script := filepath.Join(root, "delete-auth"), filepath.Join(root, "codex")
	command := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestBrokerFakeAppServer -fake-marker=%s -- \"$@\"\n", strconv.Quote(os.Args[0]), strconv.Quote(marker))
	if err := os.WriteFile(script, []byte(command), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := config.Config{RuntimeDir: filepath.Join(root, "run"), CodexExecutable: script, CodexVersion: "test", ProcessStartConcurrency: 1, UsageRefreshConcurrency: 1, WindowPulseConcurrency: 1, AuthConcurrency: 1}
	service := NewService(database, settings, secrets, codex.NewRuntimeManager(settings, secrets), events.New(100, 10))
	authHome := filepath.Join(root, "auth")
	if err := os.MkdirAll(authHome, 0o700); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": testJWT(time.Now().Add(time.Minute).Unix()), "refresh_token": "never-return"}})
	if err := os.WriteFile(filepath.Join(authHome, "auth.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Capture(authHome, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := database.Write(ctx, func(db store.Executor) error {
		if _, err := db.ExecContext(ctx, "INSERT INTO accounts VALUES('internal','public','Primary','chatgpt','CHATGPT_DEVICE_CODE','CHATGPT_DEVICE_CODE',NULL,1,'ACTIVE',?,?,NULL)", now, now); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO account_state VALUES('internal','VERIFIED','STOPPED','HEALTHY','FRESH','owner@example.com',NULL,?,?,NULL,NULL,NULL,1,?)", now, now, now); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, "INSERT INTO usage_current(account_id,short_used_percent_raw,weekly_used_percent_raw) VALUES('internal',10,40)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.installBundle(ctx, "internal", "ACTIVE", payload); err != nil {
		t.Fatal(err)
	}
	_, err = service.Lease(ctx, Account{ID: "internal", PublicID: "public"}, now, false)
	var brokerError *core.Error
	if !errors.As(err, &brokerError) || brokerError.Code != "CREDENTIAL_CHECKPOINT_FAILED" {
		t.Fatalf("checkpoint error = %v", err)
	}
	var worker, authentication, overall string
	if err := database.DB.QueryRowContext(ctx, "SELECT worker_state,auth_state,overall_state FROM account_state WHERE account_id='internal'").Scan(&worker, &authentication, &overall); err != nil {
		t.Fatal(err)
	}
	if worker != "CREDENTIAL_QUARANTINED" || authentication != "AUTH_REQUIRED" || overall != "ERROR" {
		t.Fatalf("states = %s, %s, %s", worker, authentication, overall)
	}
	var incidents int
	if err := database.DB.QueryRowContext(ctx, "SELECT count(*) FROM incidents WHERE scope_key='internal' AND problem_type='credential_checkpoint' AND state='OPEN'").Scan(&incidents); err != nil || incidents != 1 {
		t.Fatalf("open incidents = %d, %v", incidents, err)
	}
}
