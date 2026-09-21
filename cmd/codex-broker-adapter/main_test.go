package main

import "testing"

func TestLoadClientKeyFromEnvironment(t *testing.T) {
	t.Setenv("CODEX_BROKER_CLIENT_KEY", " cbk_test \n")
	key, err := loadClientKey("")
	if err != nil || key != "cbk_test" {
		t.Fatalf("key = %q, error = %v", key, err)
	}
}
