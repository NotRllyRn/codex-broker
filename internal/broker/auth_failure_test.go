package broker

import (
	"context"
	"testing"
)

func TestAuthRetryDuringCredentialUseDoesNotRequireLogin(t *testing.T) {
	for _, path := range []string{"route", "relay failure"} {
		t.Run(path, func(t *testing.T) {
			router, account := quotaFixture(t)
			// An occupied runtime cannot issue a lease. That does not establish that
			// the stored credential is invalid, even after a client reports HTTP 401.
			if _, err := router.Store.DB.Exec("UPDATE account_state SET worker_state='CREDENTIAL_IN_USE' WHERE account_id=?", account.ID); err != nil {
				t.Fatal(err)
			}
			if path == "route" {
				_, _, err := router.Route(context.Background(), "one", RouteRequest{FailedAccountID: account.PublicID, FailureKind: "auth"})
				if err != nil {
					t.Fatal(err)
				}
			} else if err := router.ReportRelayFailure(context.Background(), "one", account.PublicID, "auth"); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := router.Store.DB.QueryRow("SELECT auth_state FROM account_state WHERE account_id=?", account.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "VERIFIED" {
				t.Fatalf("temporary runtime conflict changed auth to %s", state)
			}
		})
	}
}
