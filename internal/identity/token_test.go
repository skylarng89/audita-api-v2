package identity_test

import (
	"testing"
	"time"

	"github.com/audita/audita-api/internal/identity"
	"github.com/google/uuid"
)

func newTenantUserIDs(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	return uuid.New(), uuid.New()
}

func TestNewTokenServiceRejectsInvalidPEM(t *testing.T) {
	if _, err := identity.NewTokenService("not a pem", time.Minute); err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestHashPasswordRejectsShortPassword(t *testing.T) {
	if _, err := identity.HashPassword("short"); err == nil {
		t.Fatal("expected error for < 8 char password")
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := identity.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !identity.CheckPassword(hash, "correct horse battery staple") {
		t.Fatal("expected password to verify")
	}
	if identity.CheckPassword(hash, "wrong password") {
		t.Fatal("expected wrong password to fail")
	}
}

func TestOpaqueTokenUniqueAndHashed(t *testing.T) {
	p1, h1, err := identity.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	p2, h2, err := identity.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 || h1 == h2 {
		t.Fatal("expected unique tokens")
	}
	if identity.HashOpaqueToken(p1) != h1 {
		t.Fatal("hash must match plaintext token")
	}
}

func TestKeygenProducesValidPair(t *testing.T) {
	priv, pub, err := identity.NewKeyPairPEM()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	svc, err := identity.NewTokenService(priv, time.Minute)
	if err != nil {
		t.Fatalf("token service from generated key: %v", err)
	}
	pubOut, err := svc.PublicKeyPEM()
	if err != nil {
		t.Fatalf("public key pem: %v", err)
	}
	_ = pub
	if pubOut == "" {
		t.Fatal("expected non-empty public key PEM")
	}
}
