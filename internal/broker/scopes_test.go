package broker

import (
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
)

func TestRoutingScopeFollowsAccountLabels(t *testing.T) {
	r, account := quotaFixture(t)
	ctx := t.Context()
	keys := auth.NewClientKeys(r.Store)
	if err := r.Service.SetLabels(ctx, account.PublicID, []string{"Work"}); err != nil {
		t.Fatal(err)
	}
	labels, err := keys.Labels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope := []auth.LabelScope{{ID: labels[0].ID, Mode: "allow"}}
	if err := keys.SetScopes(ctx, "one", scope); err != nil {
		t.Fatal(err)
	}
	lease, _, err := r.Route(ctx, "one", RouteRequest{})
	if err != nil || lease == nil {
		t.Fatalf("allowed lease=%+v: %v", lease, err)
	}
	if err := r.Service.SetLabels(ctx, account.PublicID, nil); err != nil {
		t.Fatal(err)
	}
	if lease, _, err := r.Route(ctx, "one", RouteRequest{PreferredAccountID: account.PublicID}); err == nil || lease != nil {
		t.Fatalf("preferred escaped scope: %+v %v", lease, err)
	}
	if lease, _, err := r.Route(ctx, "one", RouteRequest{FailedAccountID: account.PublicID, FailureKind: "auth"}); err == nil || lease != nil {
		t.Fatalf("auth retry escaped scope: %+v %v", lease, err)
	}
	labels, err = keys.Labels(ctx)
	if err != nil || len(labels) != 1 {
		t.Fatalf("scoped label was deleted: %+v %v", labels, err)
	}
	if err := r.Service.SetLabels(ctx, account.PublicID, []string{"work"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := r.accounts(ctx, "one", time.Now().UnixMilli())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("re-added label: %+v %v", accounts, err)
	}
	scope[0].Mode = "block"
	if err := keys.SetScopes(ctx, "one", scope); err != nil {
		t.Fatal(err)
	}
	accounts, err = r.accounts(ctx, "one", time.Now().UnixMilli())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("blocked account: %+v %v", accounts, err)
	}
	if lease, _, err := r.Route(ctx, "two", RouteRequest{}); err != nil || lease == nil {
		t.Fatalf("unrestricted key changed: %+v %v", lease, err)
	}
}
