package security

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	if _, err := HashPassword("too short"); err == nil {
		t.Fatal("short password was accepted")
	}
	encoded, err := HashPassword("a very good household password")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(encoded, "a very good household password") {
		t.Fatal("correct password was rejected")
	}
	if CheckPassword(encoded, "the wrong household password") {
		t.Fatal("incorrect password was accepted")
	}
	if CheckPassword("not-a-hash", "anything") {
		t.Fatal("malformed hash was accepted")
	}
}

func TestCheckPasswordRejectsMalformedArgon2Parameters(t *testing.T) {
	for _, encoded := range []string{
		"",
		"not-a-hash",
		"$argon2id$v=18$m=65536,t=3,p=2$c2FsdA$YWJj",
		"$argon2id$v=19$m=bad,t=3,p=2$c2FsdA$YWJj",
		"$argon2id$v=19$m=65536,t=3,p=2$%%%$YWJj",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$YWJj",
	} {
		if CheckPassword(encoded, "anything sufficiently long") {
			t.Fatalf("malformed Argon2 hash was accepted: %q", encoded)
		}
	}
}

func TestDummyPasswordHashIsValid(t *testing.T) {
	if !CheckPassword(DummyPasswordHash, "artisttrackarr-invalid-password") {
		t.Fatal("dummy password hash did not follow the Argon2id verification path")
	}
	if CheckPassword(DummyPasswordHash, "") {
		t.Fatal("dummy password hash accepted an empty password")
	}
}

func TestCipherRoundTripAndTamper(t *testing.T) {
	cipher, err := NewCipher("this is at least thirty two characters long")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("ntfy://secret@example/topic")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cipher.Decrypt(encrypted)
	if err != nil || plain != "ntfy://secret@example/topic" {
		t.Fatalf("round trip failed: %q %v", plain, err)
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := cipher.Decrypt(encrypted); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	if _, err := cipher.Decrypt([]byte("short")); err == nil {
		t.Fatal("truncated ciphertext was accepted")
	}
}

func TestSignedToken(t *testing.T) {
	signed := SignedToken("server secret", "session value")
	if value, ok := VerifySignedToken("server secret", signed); !ok || value != "session value" {
		t.Fatal("valid signed token was rejected")
	}
	if _, ok := VerifySignedToken("different secret", signed); ok {
		t.Fatal("token validated under a different secret")
	}
}

func TestTokenAndDigest(t *testing.T) {
	token, err := Token(24)
	if err != nil || len(token) < 20 || strings.ContainsAny(token, "=+/\n") {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if len(Digest("artisttrackarr")) != 32 || string(Digest("artisttrackarr")) == string(Digest("different")) {
		t.Fatal("digest was not a stable 256-bit distinction")
	}
}
