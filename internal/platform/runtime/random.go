package runtime

import (
	"crypto/rand"
	"encoding/hex"
)

// randRead and hexEncode are seams keeping crypto imports in one place.
func randRead(b []byte) (int, error) { return rand.Read(b) }
func hexEncode(b []byte) string      { return hex.EncodeToString(b) }