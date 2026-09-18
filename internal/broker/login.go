package broker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

type BrowserContract struct {
	Scheme, Host, Path string
	Port               int
	StateHash          []byte
}

func loginMethod(method string) (string, error) {
	if method == "" {
		return "CHATGPT_DEVICE_CODE", nil
	}
	if method == "MANUAL_TOKENS" {
		return "", core.NewError("LOGIN_METHOD_UNAVAILABLE", "Manual token import is retired; use device code or browser sign-in", 409)
	}
	if method != "CHATGPT_DEVICE_CODE" && method != "CHATGPT_BROWSER" {
		return "", core.NewError("VALIDATION_ERROR", "The login method is invalid", 422)
	}
	return method, nil
}

func BrowserAuthorizationContract(value string, ports []int, maximum int) (*BrowserContract, error) {
	if len(value) > maximum {
		return nil, core.NewError("CODEX_BROWSER_AUTH_CONTRACT_CHANGED", "Codex returned an oversized browser sign-in contract", 409)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return nil, contractError()
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query["redirect_uri"]) != 1 || len(query["state"]) != 1 {
		return nil, contractError()
	}
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || (redirect.Hostname() != "localhost" && redirect.Hostname() != "127.0.0.1") || redirect.Path != "/auth/callback" || redirect.User != nil || redirect.Fragment != "" {
		return nil, contractError()
	}
	port, err := strconv.Atoi(redirect.Port())
	if err != nil || !hasPort(ports, port) {
		return nil, contractError()
	}
	digest := sha256.Sum256([]byte(query.Get("state")))
	return &BrowserContract{"http", redirect.Hostname(), redirect.Path, port, digest[:]}, nil
}
func contractError() error {
	return core.NewError("CODEX_BROWSER_AUTH_CONTRACT_CHANGED", "Codex returned an unsupported browser sign-in contract", 409)
}
func hasPort(values []int, value int) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func ValidateCallback(value string, contract BrowserContract, maximum int) (string, error) {
	if len(value) > maximum {
		return "", core.NewError("BROWSER_CALLBACK_INVALID", "The callback URL is too large", 400)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != contract.Scheme || parsed.Hostname() != contract.Host || parsed.Port() != strconv.Itoa(contract.Port) || parsed.Path != contract.Path || parsed.User != nil || parsed.Fragment != "" {
		return "", core.NewError("BROWSER_CALLBACK_INVALID", "The callback URL is not valid", 400)
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query["code"]) != 1 || len(query["state"]) != 1 {
		return "", core.NewError("BROWSER_CALLBACK_INVALID", "The callback URL is not valid", 400)
	}
	digest := sha256.Sum256([]byte(query.Get("state")))
	if subtle.ConstantTimeCompare(digest[:], contract.StateHash) != 1 {
		return "", core.NewError("BROWSER_CALLBACK_STATE_MISMATCH", "The callback belongs to another sign-in", 409)
	}
	clean := url.Values{"code": {query.Get("code")}, "state": {query.Get("state")}}
	return contract.Scheme + "://" + contract.Host + ":" + strconv.Itoa(contract.Port) + contract.Path + "?" + clean.Encode(), nil
}

func (s *Service) StartLogin(ctx context.Context, public, method, session string, recover bool, enrollmentID string) (map[string]string, error) {
	account, err := s.account(ctx, public)
	if err != nil {
		return nil, err
	}
	if account.WorkerState == "CREDENTIAL_QUARANTINED" && !recover {
		return nil, core.NewError("CREDENTIAL_RUNTIME_BLOCKED", "Explicit reauthentication is required to recover this credential", 409)
	}
	method, err = loginMethod(method)
	if err != nil {
		return nil, err
	}
	if method == "CHATGPT_BROWSER" && s.Config.BrowserOAuthMode == "disabled" {
		return nil, core.NewError("LOGIN_METHOD_UNAVAILABLE", "Browser sign-in is disabled for this deployment", 409)
	}
	operation, err := s.CreateOperation(ctx, account.ID, "login."+strings.ToLower(method), "USER")
	if err != nil {
		return nil, err
	}
	attempt, nonce, now := core.NewID(), core.RandomToken(32), core.NowMS()
	err = s.Store.Write(ctx, func(db store.Executor) error {
		if recover {
			_, _ = db.ExecContext(ctx, "UPDATE account_state SET worker_state='STOPPED',updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND auth_state='AUTH_REQUIRED' AND worker_state='CREDENTIAL_QUARANTINED'", now, account.ID)
		}
		_, err := db.ExecContext(ctx, "INSERT INTO login_attempts VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", attempt, account.ID, operation, method, "CREATED", nil, auth.Digest(session), auth.Digest(nonce), nil, nil, now, nil, now+int64(s.Config.LoginTimeoutSeconds)*1000, nil, nil, nullString(account.Workspace), nil, nil, nil, nil, now, now)
		if err != nil {
			return err
		}
		if enrollmentID != "" {
			_, err = db.ExecContext(ctx, "INSERT INTO public_enrollments VALUES(?,?,?,?,?,?,?,?,?)", enrollmentID, account.ID, attempt, auth.Digest(session), "ACTIVE", nil, now, now+int64(s.Config.LoginTimeoutSeconds)*1000, nil)
		}
		return err
	})
	if err != nil {
		var active int
		_ = s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM login_attempts WHERE account_id=? AND state IN('CREATED','STARTING_RUNTIME','STARTING_LOGIN','WAITING_FOR_USER','OAUTH_COMPLETED','VERIFYING_ACCOUNT','STARTING_EXPORT_LOGIN','WAITING_FOR_EXPORT_USER','VERIFYING_EXPORT','FORKING_CREDENTIALS','QUIESCING_RUNTIME','CHECKPOINTING_CREDENTIAL','CANCEL_REQUESTED')", account.ID).Scan(&active)
		if active > 0 {
			_ = s.FailOperation(ctx, operation, "LOGIN_ALREADY_ACTIVE", "Another sign-in is active")
			return nil, core.NewError("LOGIN_ALREADY_ACTIVE", "Another sign-in is already active for this account", 409)
		}
		_ = s.FailOperation(ctx, operation, "LOGIN_STORAGE_CONSTRAINT", "Sign-in storage rejected the request")
		return nil, core.NewError("LOGIN_STORAGE_CONSTRAINT", "Sign-in could not be recorded; apply database migrations and retry", 500)
	}
	loginContext, cancel := context.WithCancel(s.workerCtx)
	s.mu.Lock()
	s.loginCancels[attempt] = cancel
	s.mu.Unlock()
	if !s.runAsync(func() { s.runLogin(loginContext, account, operation, attempt, method, session, nonce) }) {
		cancel()
		s.mu.Lock()
		delete(s.loginCancels, attempt)
		s.mu.Unlock()
		_ = s.FailOperation(context.Background(), operation, "SERVICE_STOPPING", "Service is stopping")
		return nil, core.NewError("SERVICE_STOPPING", "Service is stopping", 503)
	}
	return map[string]string{"operation_id": operation, "login_attempt_id": attempt, "interaction_nonce": nonce}, nil
}

func (s *Service) runLogin(ctx context.Context, account Account, operation, attempt, method, session, nonce string) {
	defer func() {
		s.mu.Lock()
		delete(s.loginCancels, attempt)
		s.mu.Unlock()
	}()
	select {
	case s.authSlots <- struct{}{}:
	case <-ctx.Done():
		var state string
		_ = s.Store.DB.QueryRowContext(context.Background(), "SELECT state FROM login_attempts WHERE login_attempt_id=?", attempt).Scan(&state)
		if state != "CANCELLED" {
			_ = s.failLogin(context.Background(), account, attempt, operation, "RESTART_REQUIRED", "LOGIN_RESTART_REQUIRED", "Sign-in was interrupted")
		}
		return
	}
	defer func() { <-s.authSlots }()
	if method == "CHATGPT_BROWSER" {
		s.browserLogin.Lock()
		defer s.browserLogin.Unlock()
	}
	lock := s.credentialLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	fail := func(err error) {
		var state string
		_ = s.Store.DB.QueryRowContext(context.Background(), "SELECT state FROM login_attempts WHERE login_attempt_id=?", attempt).Scan(&state)
		if state == "CANCELLED" {
			return
		}
		if ctx.Err() != nil {
			_ = s.failLogin(context.Background(), account, attempt, operation, "RESTART_REQUIRED", "LOGIN_RESTART_REQUIRED", "Sign-in was interrupted")
			return
		}
		code := "LOGIN_FAILED"
		failureState := "FAILED_RETRYABLE"
		var typed *core.Error
		if errors.As(err, &typed) {
			code = typed.Code
			if code == "AUTH_IDENTITY_UNVERIFIED" || code == "AUTH_IDENTITY_MISMATCH" || code == "CODEX_BROWSER_AUTH_CONTRACT_CHANGED" {
				failureState = "FAILED_ACTION_REQUIRED"
			}
		}
		_ = s.failLogin(context.WithoutCancel(ctx), account, attempt, operation, failureState, code, err.Error())
	}
	if err := s.OperationState(ctx, operation, "RUNNING", "STARTING_RUNTIME", "Starting isolated Codex runtime"); err != nil {
		fail(err)
		return
	}
	_ = s.loginState(ctx, attempt, "STARTING_RUNTIME", "")
	runtime, err := s.Runtime.Start(ctx, account.ID, nil, pointerString(account.Workspace))
	if err != nil {
		fail(err)
		return
	}
	safe, authenticated := false, false
	defer func() {
		if safe {
			_ = s.Runtime.Discard(runtime)
		} else if authenticated {
			s.Runtime.Preserve(runtime)
		} else {
			_ = runtime.Client.Close()
			_ = s.Runtime.Discard(runtime)
		}
	}()
	_ = s.loginState(ctx, attempt, "STARTING_LOGIN", "")
	interaction, err := runtime.Adapter.StartLogin(ctx, method)
	if err != nil {
		fail(err)
		return
	}
	var contract *BrowserContract
	if interaction.AuthURL != "" {
		contract, err = BrowserAuthorizationContract(interaction.AuthURL, s.Config.BrowserOAuthCallbackPorts, s.Config.BrowserCallbackMaxBytes)
		if err != nil {
			fail(err)
			return
		}
	}
	s.mu.Lock()
	s.interactions[attempt] = Interaction{attempt, auth.Digest(session), auth.Digest(nonce), interaction, contract, false}
	s.mu.Unlock()
	callbackMode := any(nil)
	callbackPort := any(nil)
	if contract != nil {
		callbackPort = contract.Port
		if s.Config.BrowserOAuthMode == "host-loopback" {
			callbackMode = "AUTOMATIC_LOOPBACK"
		} else {
			callbackMode = "MANUAL_FORWARD"
		}
	}
	now := core.NowMS()
	err = s.Store.Write(ctx, func(db store.Executor) error {
		result, err := db.ExecContext(ctx, "UPDATE login_attempts SET state='WAITING_FOR_USER',upstream_login_id=?,callback_port=?,callback_mode=?,started_at_ms=?,updated_at_ms=? WHERE login_attempt_id=? AND state='STARTING_LOGIN'", interaction.LoginID, callbackPort, callbackMode, now, now, attempt)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return core.NewError("LOGIN_CANCELLED", "Sign-in was cancelled before interaction", 409)
		}
		_, err = db.ExecContext(ctx, "UPDATE operations SET state='WAITING_FOR_USER',progress_code='WAITING_FOR_USER',progress_summary='Complete ChatGPT sign-in',state_version=state_version+1 WHERE operation_id=?", operation)
		return err
	})
	if err != nil {
		fail(err)
		return
	}
	s.Events.Publish("login.updated", map[string]any{"attempt_id": attempt, "account_id": account.PublicID, "state": "WAITING_FOR_USER", "interaction_ready": true})
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.LoginTimeoutSeconds)*time.Second)
	defer cancel()
	if err = s.waitLogin(waitCtx, runtime, interaction.LoginID); err != nil {
		fail(err)
		return
	}
	authenticated = true
	s.mu.Lock()
	delete(s.interactions, attempt)
	s.mu.Unlock()
	if err = s.loginState(ctx, attempt, "VERIFYING_ACCOUNT", "WAITING_FOR_USER"); err != nil {
		fail(err)
		return
	}
	identity, err := runtime.Adapter.Account(ctx, false)
	if err != nil {
		fail(err)
		return
	}
	observed, err := verifyIdentity(account, identity)
	if err != nil {
		fail(err)
		return
	}
	if err = s.reservePublicIdentity(ctx, account, attempt, observed); err != nil {
		fail(err)
		return
	}
	if err = runtime.Client.Close(); err != nil {
		fail(err)
		return
	}
	source, err := s.Vault.Capture(runtime.CodexHome(), s.Config.CodexVersion, pointerString(account.Workspace))
	if err != nil {
		fail(err)
		return
	}
	if err = s.promoteLogin(ctx, account.ID, attempt, source); err != nil {
		fail(err)
		return
	}
	safe = true
	_ = s.Runtime.Discard(runtime)
	sourceFingerprint, err := s.Vault.AuthFingerprint(source)
	if err != nil {
		fail(err)
		return
	}
	managedIdentity, err := s.issueCandidate(ctx, account, source, observed, "ACTIVE", map[string]bool{sourceFingerprint: true})
	if err != nil {
		fail(err)
		return
	}
	exportErr := ""
	if _, err = s.bundle(ctx, account.ID, "EXPORT"); err != nil {
		active, activeErr := s.bundle(ctx, account.ID, "ACTIVE")
		if activeErr != nil {
			fail(activeErr)
			return
		}
		activeFingerprint, activeErr := s.Vault.AuthFingerprint(active)
		if activeErr != nil {
			fail(activeErr)
			return
		}
		_, err = s.issueCandidate(ctx, account, source, observed, "EXPORT", map[string]bool{sourceFingerprint: true, activeFingerprint: true})
		if err != nil {
			exportErr = "EXPORT_FORK_FAILED"
		}
	}
	if err = s.commitLogin(ctx, account, operation, attempt, method, managedIdentity, exportErr); err != nil {
		fail(err)
		return
	}
	_, _ = s.Refresh(ctx, account.PublicID, "LOGIN")
}

func (s *Service) waitLogin(ctx context.Context, runtime *codex.Runtime, loginID string) error {
	for {
		select {
		case <-ctx.Done():
			return core.NewError("LOGIN_EXPIRED", "Sign-in expired", 409)
		case <-runtime.Client.Done():
			return runtime.Client.Err()
		case message := <-runtime.Client.Notifications():
			if stringValue(message["method"]) != "account/login/completed" {
				continue
			}
			params, _ := message["params"].(map[string]any)
			if stringValue(params["loginId"]) != loginID {
				return core.NewError("ACCOUNT_ISOLATION_VIOLATION", "Codex routed a sign-in event to the wrong account", 500)
			}
			success, _ := params["success"].(bool)
			if !success {
				return core.NewError("LOGIN_DENIED", "ChatGPT sign-in was not approved", 409)
			}
			return nil
		}
	}
}

func verifyIdentity(account Account, identity map[string]any) (map[string]any, error) {
	observed, _ := identity["account"].(map[string]any)
	if observed == nil {
		return nil, core.NewError("CODEX_AUTH_REQUIRED", "Codex authentication must be renewed", 409)
	}
	if stringValue(observed["type"]) != "chatgpt" {
		return nil, core.NewError("AUTH_IDENTITY_UNVERIFIED", "Codex did not return a ChatGPT identity", 409)
	}
	email := stringValue(observed["email"])
	if account.Email.Valid && email != "" && !strings.EqualFold(account.Email.String, email) {
		return nil, core.NewError("AUTH_IDENTITY_MISMATCH", "The authenticated ChatGPT identity does not match this account", 409)
	}
	return observed, nil
}

func (s *Service) promoteLogin(ctx context.Context, accountID, attempt string, payload vault.Payload) error {
	envelope, err := s.Vault.Encrypt(accountID, payload)
	if err != nil {
		return err
	}
	now := core.NowMS()
	return s.Store.Write(ctx, func(db store.Executor) error {
		result, err := db.ExecContext(ctx, "UPDATE login_attempts SET state='CHECKPOINTING_CREDENTIAL',updated_at_ms=? WHERE login_attempt_id=? AND state='VERIFYING_ACCOUNT'", now, attempt)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return core.NewError("LOGIN_CANCELLED", "Sign-in was cancelled before checkpointing", 409)
		}
		if _, err = db.ExecContext(ctx, "UPDATE credential_bundles SET state='RETIRED',retired_at_ms=? WHERE account_id=? AND state='ACTIVE'", now, accountID); err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", envelope.BundleID, envelope.AccountID, "ACTIVE", envelope.EnvelopeVersion, envelope.PayloadSchemaVersion, envelope.KeyID, envelope.Nonce, envelope.Ciphertext, envelope.AAD, s.Config.CodexVersion, now, now, nil)
		return err
	})
}

func (s *Service) issueCandidate(ctx context.Context, account Account, source vault.Payload, sourceIdentity map[string]any, state string, forbidden map[string]bool) (map[string]any, error) {
	if err := s.setCredentialUse(ctx, account.ID, true, true); err != nil {
		return nil, err
	}
	runtime, err := s.Runtime.Start(ctx, account.ID, &source, pointerString(account.Workspace))
	if err != nil {
		_ = s.setCredentialUse(ctx, account.ID, false, true)
		return nil, err
	}
	safe, completed := false, false
	defer func() {
		if completed {
			_ = s.Runtime.Discard(runtime)
			safe = true
		} else {
			closeErr := runtime.Client.Close()
			if state == "EXPORT" && closeErr == nil {
				s.Runtime.Archive(runtime)
				safe = true
			} else {
				s.Runtime.Preserve(runtime)
			}
		}
		_ = s.setCredentialUse(context.WithoutCancel(ctx), account.ID, false, safe)
	}()
	identity, err := runtime.Adapter.Account(ctx, true)
	if err != nil {
		return nil, err
	}
	expected := account
	if email := stringValue(sourceIdentity["email"]); email != "" {
		expected.Email = sql.NullString{String: email, Valid: true}
	}
	if _, err = verifyIdentity(expected, identity); err != nil {
		return nil, err
	}
	if err = runtime.Client.Close(); err != nil {
		return nil, err
	}
	payload, err := s.Vault.Capture(runtime.CodexHome(), s.Config.CodexVersion, pointerString(account.Workspace))
	if err != nil {
		return nil, err
	}
	fingerprint, err := s.Vault.AuthFingerprint(payload)
	if err != nil || forbidden[fingerprint] {
		return nil, core.NewError("CODEX_TOKEN_NOT_ROTATED", "Codex did not create a separate credential", 409)
	}
	if state == "ACTIVE" {
		err = s.replaceActive(ctx, account.ID, payload)
	} else {
		err = s.installBundle(ctx, account.ID, state, payload)
	}
	if err != nil {
		return nil, err
	}
	completed = true
	return identity, nil
}

func (s *Service) commitLogin(ctx context.Context, account Account, operation, attempt, method string, identity map[string]any, exportError string) error {
	observed, err := verifyIdentity(account, identity)
	if err != nil {
		return err
	}
	email, plan := stringValue(observed["email"]), stringValue(observed["planType"])
	now := core.NowMS()
	err = s.Store.Write(ctx, func(db store.Executor) error {
		result, err := db.ExecContext(ctx, "UPDATE login_attempts SET state='COMPLETED',observed_email=?,observed_plan_type=?,oauth_completed_at_ms=?,completed_at_ms=?,updated_at_ms=? WHERE login_attempt_id=? AND state='CHECKPOINTING_CREDENTIAL'", nullString(email), nullString(plan), now, now, now, attempt)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return core.NewError("LOGIN_CANCELLED", "Sign-in did not own the credential checkpoint", 409)
		}
		var enrollmentEmail sql.NullString
		_ = db.QueryRowContext(ctx, "SELECT observed_email FROM public_enrollments WHERE login_attempt_id=? AND state='ACTIVE'", attempt).Scan(&enrollmentEmail)
		display := any(nil)
		if enrollmentEmail.Valid {
			display = enrollmentEmail.String
		}
		if _, err = db.ExecContext(ctx, "UPDATE accounts SET display_name=COALESCE(?,display_name),enabled=1,lifecycle_state='ACTIVE',last_successful_login_method=?,updated_at_ms=? WHERE account_id=?", display, method, now, account.ID); err != nil {
			return err
		}
		if enrollmentEmail.Valid {
			_, _ = db.ExecContext(ctx, "UPDATE public_enrollments SET state='COMPLETED',completed_at_ms=? WHERE login_attempt_id=?", now, attempt)
		}
		if _, err = db.ExecContext(ctx, "UPDATE account_state SET auth_state='VERIFIED',worker_state='STOPPED',overall_state='WARNING',upstream_email=COALESCE(?,upstream_email),upstream_plan=COALESCE(?,upstream_plan),last_auth_verified_at_ms=?,last_error_code=NULL,last_error_summary=NULL,state_version=state_version+1,updated_at_ms=? WHERE account_id=?", nullString(email), nullString(plan), now, now, account.ID); err != nil {
			return err
		}
		code, summary := "COMPLETED", "Sign-in completed and credentials were checkpointed"
		if exportError != "" {
			code, summary = exportError, "Sign-in completed; export snapshot is unavailable"
		}
		errorSummary := any(nil)
		if exportError != "" {
			errorSummary = "The managed credential is safe; reauthenticate to retry export creation"
		}
		_, err = db.ExecContext(ctx, "UPDATE operations SET state='SUCCEEDED',progress_code=?,progress_summary=?,error_code=?,error_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?", code, summary, nullString(exportError), errorSummary, now, operation)
		return err
	})
	if err != nil {
		return err
	}
	_ = s.ResolveIncident(ctx, account.ID, "authentication_failed")
	exportAvailable := true
	if _, bundleErr := s.bundle(ctx, account.ID, "EXPORT"); bundleErr != nil {
		exportAvailable = false
	}
	s.Events.Publish("login.updated", map[string]any{"attempt_id": attempt, "account_id": account.PublicID, "state": "COMPLETED", "export_available": exportAvailable})
	return nil
}

func (s *Service) failLogin(ctx context.Context, account Account, attempt, operation, failureState, code, summary string) error {
	s.mu.Lock()
	delete(s.interactions, attempt)
	s.mu.Unlock()
	summary, _ = logbook.Redact(summary).(string)
	summary = truncateSummary(summary)
	now := core.NowMS()
	incidentAccount := ""
	err := s.Store.Write(ctx, func(db store.Executor) error {
		var authState string
		_ = db.QueryRowContext(ctx, "SELECT auth_state FROM account_state WHERE account_id=?", account.ID).Scan(&authState)
		result, err := db.ExecContext(ctx, "UPDATE login_attempts SET state=?,error_code=?,error_summary=?,completed_at_ms=?,updated_at_ms=? WHERE login_attempt_id=? AND state NOT IN('COMPLETED','CANCELLED','EXPIRED','FAILED_RETRYABLE','FAILED_ACTION_REQUIRED','RESTART_REQUIRED','SUPERSEDED')", failureState, code, summary, now, now, attempt)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if _, err := db.ExecContext(ctx, "UPDATE operations SET state='FAILED',error_code=?,error_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=? AND state NOT IN('SUCCEEDED','FAILED','CANCELLED')", code, summary, now, operation); err != nil {
			return err
		}
		if changed == 0 {
			return nil
		}
		var hasCredential, publicEnrollment int
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM credential_bundles WHERE account_id=? AND state='ACTIVE'", account.ID).Scan(&hasCredential)
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM public_enrollments WHERE login_attempt_id=? AND state='ACTIVE'", attempt).Scan(&publicEnrollment)
		if publicEnrollment > 0 {
			_, _ = db.ExecContext(ctx, "UPDATE public_enrollments SET state='FAILED',completed_at_ms=? WHERE login_attempt_id=?", now, attempt)
			if hasCredential == 0 {
				_, _ = db.ExecContext(ctx, "UPDATE accounts SET enabled=0,lifecycle_state='DELETED',deleted_at_ms=?,updated_at_ms=? WHERE account_id=?", now, now, account.ID)
			}
		}
		remainsVerified := hasCredential > 0 && authState == "VERIFIED"
		auth, overall := "AUTH_REQUIRED", "ACTION_REQUIRED"
		if remainsVerified {
			auth, overall = "VERIFIED", "WARNING"
		}
		if _, err := db.ExecContext(ctx, "UPDATE account_state SET auth_state=?,overall_state=?,last_error_code=?,last_error_summary=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?", auth, overall, code, summary, now, account.ID); err != nil {
			return err
		}
		if publicEnrollment == 0 || hasCredential > 0 {
			incidentAccount = account.ID
		}
		return nil
	})
	if err != nil {
		return err
	}
	if incidentAccount != "" {
		s.Events.Publish("login.updated", map[string]any{"attempt_id": attempt, "state": failureState, "error_code": code})
		_ = s.OpenIncident(ctx, incidentAccount, "authentication_failed", "ERROR", summary)
	}
	return nil
}

func (s *Service) loginState(ctx context.Context, attempt, state, expected string) error {
	return s.Store.Write(ctx, func(db store.Executor) error {
		query := "UPDATE login_attempts SET state=?,updated_at_ms=? WHERE login_attempt_id=?"
		args := []any{state, core.NowMS(), attempt}
		if expected != "" {
			query += " AND state=?"
			args = append(args, expected)
		}
		result, err := db.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return core.NewError("LOGIN_CANCELLED", "Sign-in is no longer active", 409)
		}
		return nil
	})
}

func (s *Service) Interaction(attempt, session, nonce string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.interactions[attempt]
	if !ok || stored.Consumed || !sameDigest(stored.SessionHash, auth.Digest(session)) || !sameDigest(stored.NonceHash, auth.Digest(nonce)) {
		return nil, core.NewError("LOGIN_INTERACTION_NOT_READY", "The sign-in interaction is unavailable", 404)
	}
	mode := any(nil)
	if stored.Contract != nil {
		mode = "MANUAL_FORWARD"
		if s.Config.BrowserOAuthMode == "host-loopback" {
			mode = "AUTOMATIC_LOOPBACK"
		}
	}
	return map[string]any{"attempt_id": attempt, "method": stored.Login.Method, "authorization_url": emptyNil(stored.Login.AuthURL), "verification_url": emptyNil(stored.Login.VerificationURL), "user_code": emptyNil(stored.Login.UserCode), "callback_mode": mode, "expires_at_ms": stored.Login.ExpiresAtMS}, nil
}
func sameDigest(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

func (s *Service) ForwardCallback(ctx context.Context, attempt, session, nonce, callback string) error {
	s.mu.Lock()
	stored, ok := s.interactions[attempt]
	if !ok || stored.Consumed || stored.Contract == nil || !sameDigest(stored.SessionHash, auth.Digest(session)) || !sameDigest(stored.NonceHash, auth.Digest(nonce)) {
		s.mu.Unlock()
		return core.NewError("LOGIN_INTERACTION_ALREADY_CONSUMED", "This callback cannot be used", 409)
	}
	destination, err := ValidateCallback(callback, *stored.Contract, s.Config.BrowserCallbackMaxBytes)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	stored.Consumed = true
	s.interactions[attempt] = stored
	s.mu.Unlock()
	client := http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, destination, nil)
	request.Header.Set("User-Agent", "codex-broker/0.1")
	response, err := client.Do(request)
	if err != nil {
		return core.NewError("BROWSER_CALLBACK_FORWARD_FAILED", "Codex did not accept the callback", 502)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode >= 400 {
		return core.NewError("BROWSER_CALLBACK_FORWARD_FAILED", "Codex did not accept the callback", 502)
	}
	return nil
}

func (s *Service) CancelLogin(ctx context.Context, attempt, session string) (string, error) {
	s.mu.Lock()
	stored, ok := s.interactions[attempt]
	if !ok || !sameDigest(stored.SessionHash, auth.Digest(session)) {
		s.mu.Unlock()
		return "", core.NewError("LOGIN_INTERACTION_SESSION_MISMATCH", "This sign-in belongs to another session", 403)
	}
	delete(s.interactions, attempt)
	s.mu.Unlock()
	var accountID, upstream, original string
	err := s.Store.Write(ctx, func(db store.Executor) error {
		err := db.QueryRowContext(ctx, "SELECT account_id,upstream_login_id,operation_id FROM login_attempts WHERE login_attempt_id=?", attempt).Scan(&accountID, &upstream, &original)
		if err != nil {
			return err
		}
		result, err := db.ExecContext(ctx, "UPDATE login_attempts SET state='CANCEL_REQUESTED',updated_at_ms=? WHERE login_attempt_id=? AND state IN('CREATED','STARTING_RUNTIME','STARTING_LOGIN','WAITING_FOR_USER','OAUTH_COMPLETED','VERIFYING_ACCOUNT')", core.NowMS(), attempt)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return core.NewError("LOGIN_NOT_CANCELLABLE", "Sign-in is no longer cancellable", 409)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	operation, err := s.CreateOperation(ctx, accountID, "login.cancel", "USER")
	if err != nil {
		return "", err
	}
	if !s.runAsync(func() {
		runtime := s.Runtime.Get(accountID)
		if runtime == nil {
			_ = s.loginState(context.Background(), attempt, "FAILED_RETRYABLE", "")
			_ = s.FailOperation(context.Background(), operation, "LOGIN_RUNTIME_GONE", "The sign-in runtime is no longer available")
			return
		}
		if err := runtime.Adapter.CancelLogin(s.workerCtx, upstream); err != nil {
			_ = s.loginState(context.Background(), attempt, "FAILED_RETRYABLE", "")
			_ = s.FailOperation(context.Background(), operation, "LOGIN_CANCEL_FAILED", err.Error())
			return
		}
		_ = s.loginState(context.Background(), attempt, "CANCELLED", "")
		_ = s.OperationState(context.Background(), original, "CANCELLED", "CANCELLED", "Sign-in cancelled")
		_ = s.OperationState(context.Background(), operation, "SUCCEEDED", "CANCELLED", "Sign-in cancelled")
		s.mu.Lock()
		cancel := s.loginCancels[attempt]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}) {
		_ = s.FailOperation(context.Background(), operation, "SERVICE_STOPPING", "Service is stopping")
	}
	return operation, nil
}

func (s *Service) reservePublicIdentity(ctx context.Context, account Account, attempt string, identity map[string]any) error {
	email := strings.ToLower(strings.TrimSpace(stringValue(identity["email"])))
	var active int
	err := s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM public_enrollments WHERE login_attempt_id=? AND state='ACTIVE'", attempt).Scan(&active)
	if err != nil || active == 0 {
		return err
	}
	if email == "" || !strings.Contains(email, "@") || utf8.RuneCountInString(email) > 80 {
		return core.NewError("PUBLIC_EMAIL_REQUIRED", "The authenticated ChatGPT account did not provide a usable email address", 409)
	}
	return s.Store.Write(ctx, func(db store.Executor) error {
		var duplicate int
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM accounts a JOIN account_state s USING(account_id) WHERE a.account_id<>? AND a.deleted_at_ms IS NULL AND (lower(a.display_name)=? OR lower(COALESCE(s.upstream_email,''))=?)`, account.ID, email, email).Scan(&duplicate)
		var reserved int
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM public_enrollments WHERE state='ACTIVE' AND account_id<>? AND lower(observed_email)=?", account.ID, email).Scan(&reserved)
		if duplicate > 0 || reserved > 0 {
			return core.NewError("PUBLIC_ACCOUNT_EXISTS", "This ChatGPT account is already managed or being added", 409)
		}
		_, err := db.ExecContext(ctx, "UPDATE public_enrollments SET observed_email=? WHERE login_attempt_id=? AND state='ACTIVE'", email, attempt)
		return err
	})
}

func (s *Service) StartPublicEnrollment(ctx context.Context, session string) (map[string]string, error) {
	s.publicEnrollment.Lock()
	defer s.publicEnrollment.Unlock()
	open, err := s.PublicEnrollmentOpen(ctx)
	if err != nil {
		return nil, err
	}
	if !open {
		return nil, core.NewError("PUBLIC_ENROLLMENT_CLOSED", "Public enrollment is closed", 404)
	}
	var active int
	now := core.NowMS()
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM public_enrollments WHERE state='ACTIVE' AND expires_at_ms>?", now).Scan(&active); err != nil {
		return nil, err
	}
	if active >= s.Config.PublicEnrollmentMaxActive {
		return nil, core.NewError("PUBLIC_ENROLLMENT_BUSY", "All sign-in slots are busy; try again later", 503)
	}
	enrollment := core.NewID()
	account, err := s.CreateAccount(ctx, "Pending sign-in "+core.RandomToken(6), nil, "", "CHATGPT_DEVICE_CODE")
	if err != nil {
		return nil, err
	}
	started, err := s.StartLogin(ctx, account["public_token"].(string), "CHATGPT_DEVICE_CODE", session, false, enrollment)
	if err != nil {
		now := core.NowMS()
		_ = s.Store.Write(ctx, func(db store.Executor) error {
			_, cleanupErr := db.ExecContext(ctx, "UPDATE accounts SET enabled=0,lifecycle_state='DELETED',deleted_at_ms=?,updated_at_ms=? WHERE account_id=?", now, now, account["account_id"])
			return cleanupErr
		})
		return nil, err
	}
	started["enrollment_id"] = enrollment
	return started, nil
}

func (s *Service) PublicEnrollmentStatus(ctx context.Context, enrollment, attempt, session, nonce string) (map[string]any, error) {
	var state, loginState string
	var email sql.NullString
	var sessionHash, nonceHash []byte
	err := s.Store.DB.QueryRowContext(ctx, "SELECT p.state,l.state,p.observed_email,p.session_hash,l.interaction_nonce_hash FROM public_enrollments p JOIN login_attempts l USING(login_attempt_id) WHERE p.enrollment_id=? AND p.login_attempt_id=?", enrollment, attempt).Scan(&state, &loginState, &email, &sessionHash, &nonceHash)
	if err != nil || !sameDigest(sessionHash, auth.Digest(session)) || !sameDigest(nonceHash, auth.Digest(nonce)) {
		return nil, core.NewError("PUBLIC_ENROLLMENT_NOT_FOUND", "Sign-in not found", 404)
	}
	if state == "COMPLETED" {
		return map[string]any{"state": "COMPLETED", "email": nullValue(email)}, nil
	}
	if state == "FAILED" || strings.HasPrefix(loginState, "FAILED") || loginState == "RESTART_REQUIRED" || loginState == "CANCELLED" || loginState == "EXPIRED" || loginState == "SUPERSEDED" {
		return map[string]any{"state": "FAILED"}, nil
	}
	interaction, err := s.Interaction(attempt, session, nonce)
	if err != nil {
		return map[string]any{"state": "STARTING"}, nil
	}
	interaction["state"] = "WAITING_FOR_USER"
	return interaction, nil
}
