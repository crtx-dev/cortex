package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSameOriginAllowsLocalAutomationAndRejectsRemoteNoOrigin is a regression
// test for a dogfooding-discovered defect: the shared automation CLI performs
// login/mutations without an Origin header, but Cortex's sameOrigin required one,
// so every CLI operation failed with 403 "origin". Loopback clients without an
// Origin are now allowed (browsers always send Origin on writes), while a
// non-loopback client without an Origin is still rejected so remote CSRF
// protection is preserved.
func TestSameOriginAllowsLocalAutomationAndRejectsRemoteNoOrigin(t *testing.T) {
	a := hardeningTestApp(t)
	handler := a.httpServer().Handler

	// First-run setup is a public unsafe-method route guarded by sameOrigin.
	body := `{"password":"mudblood","confirm":"mudblood"}`

	// Loopback client, no Origin: must be accepted (this is how the CLI works).
	local := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/setup", strings.NewReader(body))
	local.Header.Set("Content-Type", "application/json")
	local.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, local)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("loopback no-Origin request was rejected: %d", rec.Code)
	}

	// Non-loopback client, no Origin: must be rejected (CSRF preserved). The
	// Host stays loopback so the request passes host-boundary and reaches the
	// origin check; the client IP is what must reject it.
	remote := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/setup", strings.NewReader(body))
	remote.Header.Set("Content-Type", "application/json")
	remote.RemoteAddr = "203.0.113.10:12345"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, remote)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("remote no-Origin request was not rejected: %d (body=%q)", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "origin") {
		t.Fatalf("remote rejection body=%q", rec2.Body.String())
	}
}
