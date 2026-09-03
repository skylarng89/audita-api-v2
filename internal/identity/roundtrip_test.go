package identity_test

import (
	"testing"
	"time"

	"github.com/audita/audita-api/internal/identity"
	"github.com/google/uuid"
)

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	priv, _, err := identity.NewKeyPairPEM()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	svc, err := identity.NewTokenService(priv, time.Minute)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	tenantID := uuid.New()
	userID := uuid.New()

	tok, err := svc.Issue(tenantID, userID, "admin", 3)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := svc.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.TenantID != tenantID.String() {
		t.Errorf("tenantId = %s, want %s", claims.TenantID, tenantID)
	}
	if claims.UserID != userID.String() {
		t.Errorf("userId = %s, want %s", claims.UserID, userID)
	}
	if claims.Role != "admin" || claims.TokenVer != 3 {
		t.Errorf("role/tokenVersion = %s/%d, want admin/3", claims.Role, claims.TokenVer)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	priv, _, err := identity.NewKeyPairPEM()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := identity.NewTokenService(priv, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify("garbage.token.here"); err == nil {
		t.Fatal("expected verify failure for garbage token")
	}
}

func TestPublicKeyPEMRoundTrip(t *testing.T) {
	priv, pub, err := identity.NewKeyPairPEM()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := identity.NewTokenService(priv, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pubOut, err := svc.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if stripWhitespace(pubOut) != stripWhitespace(pub) {
		t.Fatal("public key PEM mismatch")
	}
}

func TestVerifyRejectsCrossSignedToken(t *testing.T) {
	privA, _, err := identity.NewKeyPairPEM()
	if err != nil {
		t.Fatal(err)
	}
	privB, _, err := identity.NewKeyPairPEM()
	if err != nil {
		t.Fatal(err)
	}
	svcA, _ := identity.NewTokenService(privA, time.Minute)
	svcB, _ := identity.NewTokenService(privB, time.Minute)

	tok, err := svcA.Issue(uuid.New(), uuid.New(), "admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svcB.Verify(tok); err == nil {
		t.Fatal("expected verification failure with different key")
	}
}

func stripWhitespace(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == ' ' {
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
