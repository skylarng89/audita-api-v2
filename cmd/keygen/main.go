// Command keygen generates an RSA-2048 keypair in PEM format for the
// JWT_PRIVATE_KEY_PEM / JWT_PUBLIC_KEY_PEM configuration values (ADR-7),
// plus a base64 32-byte APP_ENCRYPTION_KEY.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	"github.com/audita/audita-api/internal/identity"
	"github.com/audita/audita-api/internal/platform/runtime"
)
)

func main() {
	logger := runtime.NewNopLogger()
	_ = logger
	priv, pub, err := identity.NewKeyPairPEM()
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
	fmt.Println("# Add these to your .env — never commit real values")
	fmt.Println("JWT_PRIVATE_KEY_PEM='" + priv + "'")
	fmt.Println("JWT_PUBLIC_KEY_PEM='" + pub + "'")
	fmt.Println("APP_ENCRYPTION_KEY=" + base64.StdEncoding.EncodeToString(encKey))
}
