package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestForwardedHeadersRequireExplicitLoopbackProxyTrust(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://cortex.example/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := (&App{}).requestScheme(req); got != "http" {
		t.Fatalf("unconfigured scheme=%q", got)
	}
	a := &App{trustProxy: true}
	if got := a.requestScheme(req); got != "https" || a.clientIP(req) != "203.0.113.9" {
		t.Fatalf("trusted proxy scheme/ip=%q/%q", got, a.clientIP(req))
	}
	req.RemoteAddr = "198.51.100.2:1234"
	if got := a.requestScheme(req); got != "http" || a.clientIP(req) != "198.51.100.2" {
		t.Fatalf("remote forwarding was trusted: %q/%q", got, a.clientIP(req))
	}
}

func TestForwardedChainsFailClosed(t *testing.T) {
	a := &App{trustProxy: true}
	req := httptest.NewRequest(http.MethodGet, "http://cortex.example/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https, http")
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 127.0.0.1")
	if a.requestScheme(req) != "http" || a.clientIP(req) != "127.0.0.1" {
		t.Fatalf("ambiguous chain accepted: %s/%s", a.requestScheme(req), a.clientIP(req))
	}
}

func TestHostAndOriginValidation(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0", PublicOrigin: "https://cortex.example"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if !a.validHost("cortex.example") || a.validHost("evil.example") {
		t.Fatal("public host policy failed")
	}
	req := httptest.NewRequest(http.MethodPost, "http://cortex.example/api/settings", nil)
	req.Host = "cortex.example"
	for origin, want := range map[string]bool{
		"https://cortex.example":      true,
		"http://cortex.example":       false,
		"https://evil.example":        false,
		"https://cortex.example/path": false,
	} {
		req.Header.Del("Origin")
		req.Header.Add("Origin", origin)
		if got := a.sameOrigin(req); got != want {
			t.Fatalf("origin %q=%v want %v", origin, got, want)
		}
	}
	req.Header.Add("Origin", "https://cortex.example")
	if a.sameOrigin(req) {
		t.Fatal("duplicate Origin headers were accepted")
	}
}

func TestCSRFProtectsSessionMutations(t *testing.T) {
	a := hardeningTestApp(t)
	acct, err := a.accounts.initial("Administrator", "admin", "admin@example.com", "password")
	if err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/login", nil)
	created := httptest.NewRecorder()
	a.newSessionCookie(created, seed, acct.ID, acct.Identities[0].ID)
	cookie := created.Result().Cookies()[0]
	csrf := a.sessions[cookie.Value].CSRF

	request := func(origin string, headers []string) int {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/logout", nil)
		req.Header.Set("Origin", origin)
		for _, value := range headers {
			req.Header.Add("X-Cortex-CSRF", value)
		}
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		a.httpServer().Handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request("https://evil.example", []string{csrf}); got != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d", got)
	}
	if got := request("http://127.0.0.1", nil); got != http.StatusForbidden {
		t.Fatalf("missing token status=%d", got)
	}
	if got := request("http://127.0.0.1", []string{csrf, csrf}); got != http.StatusForbidden {
		t.Fatalf("duplicate token status=%d", got)
	}
	if got := request("http://127.0.0.1", []string{csrf}); got != http.StatusOK {
		t.Fatalf("valid mutation status=%d", got)
	}
}

func TestPublicLoginRejectsCrossSiteSubmission(t *testing.T) {
	a := hardeningTestApp(t)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/login", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	a.httpServer().Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site login status=%d", rec.Code)
	}
}
