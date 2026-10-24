package broker

import (
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/auth"
)

func TestLabelLifecycle(t *testing.T) {
	r, account := quotaFixture(t)
	ctx := t.Context()
	keys := auth.NewClientKeys(r.Store)
	for _, name := range []string{" Label ", "label", "Another-label"} {
		if err := r.Service.CreateLabel(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	labels, err := keys.Labels(ctx)
	if err != nil || len(labels) != 2 {
		t.Fatalf("labels=%+v err=%v", labels, err)
	}
	if err := r.Service.SetLabels(ctx, account.PublicID, []string{"Label"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Service.SetLabels(ctx, account.PublicID, nil); err != nil {
		t.Fatal(err)
	}
	labels, err = keys.Labels(ctx)
	if err != nil || len(labels) != 2 {
		t.Fatalf("unused labels lost: %+v %v", labels, err)
	}
	if err := r.Service.SetLabels(ctx, account.PublicID, []string{"Label"}); err != nil {
		t.Fatal(err)
	}
	id := labels[1].ID
	if err := keys.SetScopes(ctx, "one", []auth.LabelScope{{ID: id, Mode: "allow"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Service.DeleteLabel(ctx, id); err == nil {
		t.Fatal("deleted label used by access rule")
	}
	if got, err := r.Service.labels(ctx, account.ID); err != nil || len(got) != 1 {
		t.Fatalf("rejected deletion changed account: %v %v", got, err)
	}
	if err := keys.SetScopes(ctx, "one", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Service.DeleteLabel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Service.labels(ctx, account.ID); err != nil || len(got) != 0 {
		t.Fatalf("deleted label remains: %v %v", got, err)
	}
	for _, name := range []string{"", "  ", "a,b", "abcdefghijklmnopqrstuvwxyzabcdefghijklmno"} {
		if err := r.Service.CreateLabel(ctx, name); err == nil {
			t.Fatalf("accepted invalid label %q", name)
		}
	}
}
