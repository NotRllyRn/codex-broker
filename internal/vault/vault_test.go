package vault

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvelopeCaptureAndMaterialize(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	secrets, err := New(key, "instance-test")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"tokens":{"access_token":"value"}}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Capture(home, "codex-cli 0.145.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secrets.Encrypt("account-test", payload)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := secrets.Decrypt(envelope)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "destination")
	if err := secrets.Materialize(decrypted, destination); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(destination, "auth.json"))
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("materialized = %q, %v", actual, err)
	}
}

func TestDecryptsPythonEnvelope(t *testing.T) {
	decode := func(value string) []byte {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	secrets, err := New([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}, "fixture-instance")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Decrypt(Envelope{
		BundleID: "fixture-bundle", AccountID: "fixture-account", KeyID: "primary",
		Nonce:                decode("wCTPh5Co9q0i2Dgd"),
		Ciphertext:           decode("FIYyZTGUtOwRu+2JmQxTNvm/FXU2LqhJIDMDO+U9nnsq/KU34bNBV2hdx113YEw4essJjD7OXLVVQNbwGEfyrWKa1r1j2AK0kMLdmeRKsFjm023QSvSnvRJEa/s4aVbiDg09qd8GAg3NE8lnWdRgY+b/gd9w84upkqK2Yn/oUucfWIWeL4+JnVBUJWrrLH/qrTEMMcy318e3D3bywEdadkR/hNSZQ6wu1Y2wfrjcMtLUxAu/AJHQeMXc+jGbSaPzZ/UHxZdWZK+dZvi3vtvRQF851HxRJ3zRy88OAK9ooiYXb2+DkSVEIovf8rIXqN5lNOM2Nq8c6uv05yckRcgbgQyhNg6Nn4yKTsHL"),
		AAD:                  decode("eyJhY2NvdW50X2lkIjoiZml4dHVyZS1hY2NvdW50IiwiYnVuZGxlX2lkIjoiZml4dHVyZS1idW5kbGUiLCJlbnZlbG9wZV92ZXJzaW9uIjoxLCJpbnN0YW5jZV9pZCI6ImZpeHR1cmUtaW5zdGFuY2UiLCJrZXlfaWQiOiJwcmltYXJ5IiwicGF5bG9hZF9zY2hlbWFfdmVyc2lvbiI6MX0="),
		PayloadSchemaVersion: 1, EnvelopeVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.CodexVersion != "codex-cli 0.145.0" {
		t.Fatalf("codex version = %q", payload.CodexVersion)
	}
}
