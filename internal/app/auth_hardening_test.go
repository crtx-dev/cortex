package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentFirstRunSetupHasOneWinner(t *testing.T) {
	a := hardeningTestApp(t)
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/setup", strings.NewReader(`{"password":"mudblood","confirm":"mudblood"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://127.0.0.1")
			a.httpServer().Handler.ServeHTTP(rec, req)
			statuses <- rec.Code
		}()
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("setup statuses=%v", counts)
	}
}

func TestSessionsAreBoundedAndOldestIsEvicted(t *testing.T) {
	a := hardeningTestApp(t)
	var first string
	for i := 0; i < maxSessions+3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://cortex/api/auth/login", nil)
		a.newSessionCookie(rec, req)
		if i == 0 {
			first = rec.Result().Cookies()[0].Value
		}
		time.Sleep(time.Millisecond)
	}
	if len(a.sessions) != maxSessions {
		t.Fatalf("sessions=%d", len(a.sessions))
	}
	if _, ok := a.sessions[first]; ok {
		t.Fatal("oldest session was not evicted")
	}
}

func TestPasswordChangeRevokesOtherSessionsAndRotatesCurrent(t *testing.T) {
	a := hardeningTestApp(t)
	acct, err := a.accounts.initial("Administrator", "admin", "admin@example.com", "old-password")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://cortex/api/auth/password", nil)
	first := httptest.NewRecorder()
	a.newSessionCookie(first, request, acct.ID, acct.Identities[0].ID)
	oldCookie := first.Result().Cookies()[0]
	a.newSessionCookie(httptest.NewRecorder(), request, acct.ID, acct.Identities[0].ID)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/password", strings.NewReader(`{"Current":"old-password","Password":"new-password","Confirm":"new-password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1")
	req.Header.Set("X-Cortex-CSRF", a.sessions[oldCookie.Value].CSRF)
	req.AddCookie(oldCookie)
	rec := httptest.NewRecorder()
	a.httpServer().Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("password change=%d %s", rec.Code, rec.Body.String())
	}
	if len(a.sessions) != 1 || a.authenticated(req) {
		t.Fatalf("old sessions survived rotation: %d", len(a.sessions))
	}
	if len(rec.Result().Cookies()) == 0 || rec.Result().Cookies()[0].Value == oldCookie.Value {
		t.Fatal("current session was not rotated")
	}
}

func TestPublicAuthStateDoesNotExposeGoogleIdentity(t *testing.T) {
	a := hardeningTestApp(t)
	a.settings.Auth.GoogleClientID = "client-canary"
	a.settings.Auth.GoogleEmail = "owner@example.com"
	rec := httptest.NewRecorder()
	a.authState(rec, httptest.NewRequest(http.MethodGet, "/api/auth/state", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["googleClientID"]; ok {
		t.Fatal("public auth state exposed Google client id")
	}
	if _, ok := out["googleEmail"]; ok {
		t.Fatal("public auth state exposed configured email")
	}
}

func TestTOTPIsSingleUseAndEnrollmentExpires(t *testing.T) {
	a := hardeningTestApp(t)
	secret := "JBSWY3DPEHPK3PXP"
	code := totpCode(secret, time.Now())
	if !a.consumeTOTP(secret, code) || a.consumeTOTP(secret, code) {
		t.Fatal("TOTP replay was not rejected")
	}
	a.pendingTOTP["session"] = pendingTOTP{Secret: secret, Expires: time.Now().Add(-time.Second)}
	p := a.pendingTOTP["session"]
	if time.Now().Before(p.Expires) {
		t.Fatal("test enrollment did not expire")
	}
}

func TestOAuthStateIsBoundedSingleUseAndPKCEBound(t *testing.T) {
	a := hardeningTestApp(t)
	a.settings.Auth.GoogleEnabled = true
	a.settings.Auth.GoogleClientID = "client"
	a.settings.Auth.GoogleClientSecret = "secret"
	var state string
	for i := 0; i < maxOAuthStates+10; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://cortex.example/api/auth/google/start", nil)
		req.Host = "cortex.example"
		a.googleStart(rec, req)
		location, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if location.Query().Get("code_challenge_method") != "S256" || location.Query().Get("code_challenge") == "" {
			t.Fatal("OAuth start omitted PKCE")
		}
		state = location.Query().Get("state")
	}
	if len(a.oauthStates) != maxOAuthStates {
		t.Fatalf("OAuth states=%d", len(a.oauthStates))
	}
	a.authMu.Lock()
	pending, ok := a.oauthStates[state]
	delete(a.oauthStates, state)
	_, replay := a.oauthStates[state]
	a.authMu.Unlock()
	if !ok || replay || pending.Verifier == "" || pending.Redirect != "https://cortex.example/api/auth/google/callback" {
		t.Fatalf("invalid OAuth state lifecycle: %#v", pending)
	}
}

func TestLoginFailureStoreIsBounded(t *testing.T) {
	a := hardeningTestApp(t)
	for i := 0; i < maxLoginClients+20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = fmt.Sprintf("198.51.%d.%d:1234", (i/250)%250, i%250+1)
		a.recordLoginFailure(req)
	}
	if len(a.loginFailures) > maxLoginClients {
		t.Fatalf("login client store=%d", len(a.loginFailures))
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	for i := 0; i < loginAttempts; i++ {
		a.recordLoginFailure(req)
	}
	if a.loginAllowed(req) {
		t.Fatal("login throttle allowed excess attempt")
	}
}
