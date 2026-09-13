package app

import (
	"encoding/base64"
	"testing"

	coreauth "github.com/gantry-tools/gantry-core/auth"
)

func TestCortexLegacyPasswordHashIsNotAccepted(t *testing.T) {
	legacySalt := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef"))
	legacy := "pbkdf2-sha256$310000$" + legacySalt + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	if coreauth.VerifyPassword(legacy, "password") {
		t.Fatal("legacy Cortex base64-salt hash was accepted")
	}
}
