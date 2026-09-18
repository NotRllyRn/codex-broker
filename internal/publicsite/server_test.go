package publicsite

import (
	"encoding/base64"
	"testing"
)

func TestEnrollmentHandleRoundTripAndValidation(t *testing.T) {
	value := "abcdefghijklmnopqrstuvwxyz123456"
	handle := Handle{value, value, value, value}
	if decoded := decodeHandle(handle.Encode()); decoded == nil || *decoded != handle {
		t.Fatalf("handle did not round trip: %#v", decoded)
	}
	extra := base64.RawURLEncoding.EncodeToString([]byte(`{"enrollment_id":"abcdefghijklmnopqrstuvwxyz123456","login_attempt_id":"abcdefghijklmnopqrstuvwxyz123456","session_token":"abcdefghijklmnopqrstuvwxyz123456","interaction_nonce":"abcdefghijklmnopqrstuvwxyz123456","extra":true}`))
	if decodeHandle(extra) != nil {
		t.Fatal("handle with unknown fields was accepted")
	}
	trailing := base64.RawURLEncoding.EncodeToString([]byte(`{"enrollment_id":"abcdefghijklmnopqrstuvwxyz123456","login_attempt_id":"abcdefghijklmnopqrstuvwxyz123456","session_token":"abcdefghijklmnopqrstuvwxyz123456","interaction_nonce":"abcdefghijklmnopqrstuvwxyz123456"}{}`))
	if decodeHandle(trailing) != nil {
		t.Fatal("handle with trailing JSON was accepted")
	}
}
