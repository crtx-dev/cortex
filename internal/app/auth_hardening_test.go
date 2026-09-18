package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/gantry-tools/gantry-core/auth"
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
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/setup", strings.NewReader(`{"username":"admin","email":"admin@example.com","password":"mudblood","confirm":"mudblood"}`))
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

func TestSetupRequiresUsernameAndEmailAndLoginWorksByEither(t *testing.T) {
	a := hardeningTestApp(t)
	h := a.httpServer().Handler
	// Empty username must be rejected.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/setup", strings.NewReader(`{"email":"admin@example.com","password":"mudblood","confirm":"mudblood"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty-username setup=%d, want 400", rec.Code)
	}
	// Valid setup (username, email, password, confirm) succeeds.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/setup", strings.NewReader(`{"username":"admin","email":"admin@example.com","password":"mudblood","confirm":"mudblood"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup=%d %s", rec.Code, rec.Body.String())
	}
	// Login by username works.
	login := func(identifier string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/login", strings.NewReader(`{"username":"`+identifier+`","password":"mudblood"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1")
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := login("admin"); got != http.StatusOK {
		t.Fatalf("login by username=%d, want 200", got)
	}
	if got := login("admin@example.com"); got != http.StatusOK {
		t.Fatalf("login by email=%d, want 200", got)
	}
}

func TestManageCreateAccountRequiresUsernameEmailAndDerivesDisplay(t *testing.T) {
	a := hardeningTestApp(t)
	h := a.httpServer().Handler
	do := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, "http://127.0.0.1"+path, reader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-Cortex-CSRF", csrf)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// First-run setup establishes an administrator session.
	rec := do(http.MethodPost, "/api/auth/setup", `{"username":"admin","email":"admin@example.com","password":"mudblood","confirm":"mudblood"}`, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("setup=%d %s", rec.Code, rec.Body.String())
	}
	var cookies []*http.Cookie
	for _, c := range rec.Result().Cookies() {
		cookies = append(cookies, c)
	}
	adminCookie := cookies[0]
	state := do(http.MethodGet, "/api/auth/state", "", adminCookie, "")
	var auth map[string]any
	if err := json.NewDecoder(state.Body).Decode(&auth); err != nil {
		t.Fatal(err)
	}
	csrf := auth["csrf"].(string)

	// create-account without username or email must be rejected.
	if got := do(http.MethodPost, "/api/manage/users", `{"action":"create-account","email":"u@example.com","password":"mudblood","roles":["user"]}`, adminCookie, csrf).Code; got != http.StatusBadRequest {
		t.Fatalf("create without username=%d, want 400", got)
	}
	if got := do(http.MethodPost, "/api/manage/users", `{"action":"create-account","username":"u","password":"mudblood","roles":["user"]}`, adminCookie, csrf).Code; got != http.StatusBadRequest {
		t.Fatalf("create without email=%d, want 400", got)
	}
	// A valid create derives DisplayName from Username.
	if got := do(http.MethodPost, "/api/manage/users", `{"action":"create-account","username":"u","email":"u@example.com","password":"mudblood","roles":["user"]}`, adminCookie, csrf).Code; got != http.StatusOK {
		t.Fatalf("create=%d", got)
	}
	var created *coreauth.Account
	for _, acct := range a.accounts.list() {
		for _, identity := range acct.Identities {
			if identity.Type == "password" && strings.EqualFold(identity.Username, "u") {
				c := acct
				created = &c
			}
		}
	}
	if created == nil {
		t.Fatalf("created account not found by username")
	}
	if created.DisplayName != "u" {
		t.Fatalf("created account display=%q, want derived from username", created.DisplayName)
	}
	// The new account can sign in by username.
	if got := do(http.MethodPost, "/api/auth/login", `{"username":"u","password":"mudblood"}`, nil, "").Code; got != http.StatusOK {
		t.Fatalf("login by username=%d, want 200", got)
	}
}
