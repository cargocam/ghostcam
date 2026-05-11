package auth

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Password hashing ---

func TestHashPassword_VerifyRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("hash missing PHC prefix: %q", encoded)
	}

	ok, err := VerifyPassword("correct-horse-battery-staple", encoded)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword returned false for matching password")
	}

	ok, err = VerifyPassword("wrong-password", encoded)
	if err != nil {
		t.Fatalf("VerifyPassword (wrong): %v", err)
	}
	if ok {
		t.Error("VerifyPassword returned true for non-matching password")
	}
}

func TestHashPassword_UniqueSalt(t *testing.T) {
	// Each call must generate a fresh salt — if two hashes of the same
	// password collide, the PRNG is broken or salt generation is skipped.
	a, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword a: %v", err)
	}
	b, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword b: %v", err)
	}
	if a == b {
		t.Error("HashPassword produced identical hashes for the same input — salt is not unique")
	}
}

// TestVerifyPassword_ConcurrentDoesNotDeadlock pins that the argon2
// semaphore guarding peak transient allocation doesn't introduce a
// deadlock when verifications outnumber slots. The semaphore is sized
// at 2 (see auth.go); we run 8 verifications and expect all of them to
// complete and return correct results.
func TestVerifyPassword_ConcurrentDoesNotDeadlock(t *testing.T) {
	encoded, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, err := VerifyPassword("correct-horse-battery-staple", encoded)
			if err != nil {
				t.Errorf("VerifyPassword (goroutine %d): %v", idx, err)
				return
			}
			results[idx] = ok
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("VerifyPassword goroutines did not complete in 30s — semaphore likely deadlocked")
	}

	for i, ok := range results {
		if !ok {
			t.Errorf("VerifyPassword goroutine %d returned false", i)
		}
	}
}

func TestVerifyPassword_Malformed(t *testing.T) {
	// Malformed hashes must return a parse error, not panic or default-true.
	cases := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$v=19$m=65536,t=1,p=4$only-one-segment",
		"$argon2id$v=19$m=bad,t=1,p=4$abc$def",
	}
	for _, c := range cases {
		ok, err := VerifyPassword("anything", c)
		if err == nil {
			t.Errorf("VerifyPassword(%q) expected error, got nil (ok=%v)", c, ok)
		}
		if ok {
			t.Errorf("VerifyPassword(%q) returned ok=true for malformed hash", c)
		}
	}
}

// --- HMAC tokens ---

func TestHMACToken_Deterministic(t *testing.T) {
	secret := []byte("shared-secret")
	a := HMACToken("raw-token", secret)
	b := HMACToken("raw-token", secret)
	if a != b {
		t.Errorf("HMACToken not deterministic: %q != %q", a, b)
	}
	// Hex-encoded SHA256 = 64 chars.
	if len(a) != 64 {
		t.Errorf("HMACToken length = %d, want 64", len(a))
	}
}

func TestHMACToken_SecretSensitive(t *testing.T) {
	a := HMACToken("raw-token", []byte("secret-one"))
	b := HMACToken("raw-token", []byte("secret-two"))
	if a == b {
		t.Error("HMACToken returned identical output for different secrets")
	}
}

func TestHMACToken_InputSensitive(t *testing.T) {
	secret := []byte("shared-secret")
	a := HMACToken("token-a", secret)
	b := HMACToken("token-b", secret)
	if a == b {
		t.Error("HMACToken returned identical output for different inputs")
	}
}

// --- JWT ---

func TestJWT_RoundTrip(t *testing.T) {
	secret := GenerateHMACSecret()
	token := SignJWT("user-123", "alice@example.com", true, secret, time.Hour)

	claims := VerifyJWT(token, secret)
	if claims == nil {
		t.Fatal("VerifyJWT returned nil for a freshly-signed token")
	}
	if claims.UserID != "user-123" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-123")
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("Email = %q, want %q", claims.Email, "alice@example.com")
	}
	if !claims.IsAdmin {
		t.Error("IsAdmin = false, want true")
	}
}

func TestJWT_WrongSecret(t *testing.T) {
	token := SignJWT("user-123", "alice@example.com", false, []byte("signing-secret"), time.Hour)
	if claims := VerifyJWT(token, []byte("different-secret")); claims != nil {
		t.Errorf("VerifyJWT accepted token under wrong secret: %+v", claims)
	}
}

func TestJWT_Expired(t *testing.T) {
	secret := []byte("shared-secret")
	// Negative TTL → exp in the past.
	token := SignJWT("user-123", "alice@example.com", false, secret, -time.Hour)
	if claims := VerifyJWT(token, secret); claims != nil {
		t.Errorf("VerifyJWT accepted expired token: %+v", claims)
	}
}

func TestJWT_TamperedSignature(t *testing.T) {
	secret := []byte("shared-secret")
	token := SignJWT("user-123", "alice@example.com", false, secret, time.Hour)

	// Flip the final character of the signature segment.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	sig := parts[2]
	last := sig[len(sig)-1]
	var flipped byte
	if last == 'A' {
		flipped = 'B'
	} else {
		flipped = 'A'
	}
	parts[2] = sig[:len(sig)-1] + string(flipped)
	tampered := strings.Join(parts, ".")

	if claims := VerifyJWT(tampered, secret); claims != nil {
		t.Errorf("VerifyJWT accepted token with tampered signature: %+v", claims)
	}
}

// TestJWT_TamperedPayload is the privilege-escalation invariant: an
// attacker cannot flip `is_admin` (or any other claim) without
// invalidating the HMAC, because the signature covers header.payload.
func TestJWT_TamperedPayload(t *testing.T) {
	secret := []byte("shared-secret")
	// Sign a non-admin token.
	original := SignJWT("user-123", "alice@example.com", false, secret, time.Hour)

	// Sign an admin token under a DIFFERENT secret — swap the payload
	// back into the original and check that the HMAC no longer verifies.
	adminToken := SignJWT("user-123", "alice@example.com", true, []byte("attacker-secret"), time.Hour)

	origParts := strings.Split(original, ".")
	adminParts := strings.Split(adminToken, ".")
	if len(origParts) != 3 || len(adminParts) != 3 {
		t.Fatalf("malformed JWTs")
	}
	// Paste the admin payload into the legitimate token. Signature is
	// still the legitimate one over the non-admin payload, so HMAC mismatch.
	forged := origParts[0] + "." + adminParts[1] + "." + origParts[2]

	if claims := VerifyJWT(forged, secret); claims != nil {
		t.Errorf("VerifyJWT accepted forged payload: %+v", claims)
	}
}

func TestJWT_MalformedInputs(t *testing.T) {
	secret := []byte("shared-secret")
	cases := []string{
		"",
		"only-one-segment",
		"two.segments",
		"four.segments.here.extra",
		"not-base64!.also-not-base64!.nope!",
	}
	for _, c := range cases {
		if claims := VerifyJWT(c, secret); claims != nil {
			t.Errorf("VerifyJWT(%q) returned non-nil claims: %+v", c, claims)
		}
	}
}

// TestJWT_MissingClaims guards against a degenerate token whose HMAC
// verifies but whose payload is missing required fields (sub / exp).
// VerifyJWT must return nil rather than handing back zero-value claims
// that downstream code might treat as a valid anonymous session.
func TestJWT_MissingClaims(t *testing.T) {
	secret := []byte("shared-secret")

	// Sign a token with only email — SignJWT always includes sub/exp, so
	// we construct the payload manually via a second SignJWT call that
	// we then rewrite to strip fields. Simpler: sign with empty userID.
	token := SignJWT("", "alice@example.com", false, secret, time.Hour)
	if claims := VerifyJWT(token, secret); claims != nil {
		t.Errorf("VerifyJWT returned claims for empty sub: %+v", claims)
	}
}
