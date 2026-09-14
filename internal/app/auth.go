package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	coreauth "github.com/gantry-tools/gantry-core/auth"
)

func (a *App) trustedProxy(r *http.Request) bool {
	if !a.trustProxy {
		return false
	}
	ip := net.ParseIP(directClientIP(r))
	return ip != nil && ip.IsLoopback()
}

func (a *App) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if a.trustedProxy(r) {
		raw := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
		if !strings.Contains(raw, ",") {
			if proto := strings.ToLower(raw); proto == "https" || proto == "http" {
				return proto
			}
		}
	}
	return "http"
}

func (a *App) clientIP(r *http.Request) string {
	if a.trustedProxy(r) {
		raw := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
		if !strings.Contains(raw, ",") {
			if ip := net.ParseIP(raw); ip != nil {
				return ip.String()
			}
		}
	}
	return directClientIP(r)
}

func (a *App) validHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	if a.publicOrigin != nil {
		return strings.EqualFold(hostport, a.publicOrigin.Host)
	}
	return false
}

func (a *App) sameOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		// Local automation clients (the shared CLI, curl, scripts) never send an
		// Origin header. Browsers always send Origin on writes, so this
		// exemption is only reachable by a loopback client, preserving CSRF
		// protection for every remote browser.
		if !a.validHost(r.Host) {
			return false
		}
		ip := net.ParseIP(a.clientIP(r))
		return ip != nil && ip.IsLoopback()
	}
	if len(values) != 1 {
		return false
	}
	u, err := url.Parse(values[0])
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	expectedScheme, expectedHost := a.requestScheme(r), r.Host
	if a.publicOrigin != nil {
		expectedScheme, expectedHost = a.publicOrigin.Scheme, a.publicOrigin.Host
	}
	return strings.EqualFold(u.Scheme, expectedScheme) && strings.EqualFold(u.Host, expectedHost)
}

type AuthSettings struct {
	TOTPSecrets        map[string]string `json:"totpSecrets,omitempty"`
	GoogleEnabled      bool              `json:"googleEnabled,omitempty"`
	GoogleClientID     string            `json:"googleClientId,omitempty"`
	GoogleClientSecret string            `json:"googleClientSecret,omitempty"`
	GoogleEmail        string            `json:"googleEmail,omitempty"`
}
type sessionInfo struct {
	Created    time.Time
	Expires    time.Time
	CSRF       string
	AccountID  string
	IdentityID string
}
type pendingTOTP struct {
	Secret  string
	Expires time.Time
}
type oauthState struct {
	Expires  time.Time
	Verifier string
	Redirect string
}

const (
	maxSessions     = 8
	maxLoginClients = 1024
	maxOAuthStates  = 128
	loginWindow     = 5 * time.Minute
	loginAttempts   = 5
)

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// PBKDF2-HMAC-SHA256 avoids storing a plaintext password and uses only the Go standard library.
func passwordHash(password string) string {
	hash, _ := coreauth.HashPassword(password)
	return hash
}
func verifyPassword(stored, password string) bool {
	return coreauth.VerifyPassword(stored, password)
}
func pbkdf2SHA256(password, salt []byte, iter, n int) []byte {
	out := make([]byte, 0, n)
	for block := uint32(1); len(out) < n; block++ {
		b := make([]byte, len(salt)+4)
		copy(b, salt)
		binary.BigEndian.PutUint32(b[len(salt):], block)
		m := hmac.New(sha256.New, password)
		m.Write(b)
		u := m.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			m = hmac.New(sha256.New, password)
			m.Write(u)
			u = m.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:n]
}
func totpCode(secret string, now time.Time) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(now.Unix()/30))
	m := hmac.New(sha1.New, key)
	m.Write(b[:])
	sum := m.Sum(nil)
	o := sum[len(sum)-1] & 15
	v := (uint32(sum[o])&127)<<24 | uint32(sum[o+1])<<16 | uint32(sum[o+2])<<8 | uint32(sum[o+3])
	return fmt.Sprintf("%06d", v%1000000)
}
func verifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	now := time.Now()
	for d := -1; d <= 1; d++ {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, now.Add(time.Duration(d)*30*time.Second))), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

func (a *App) consumeTOTP(secret, code string) bool {
	if !verifyTOTP(secret, code) {
		return false
	}
	sum := sha256.Sum256([]byte(secret + "\x00" + strings.TrimSpace(code)))
	key := hex.EncodeToString(sum[:])
	now := time.Now()
	a.authMu.Lock()
	defer a.authMu.Unlock()
	for id, expires := range a.usedTOTP {
		if now.After(expires) {
			delete(a.usedTOTP, id)
		}
	}
	if _, used := a.usedTOTP[key]; used {
		return false
	}
	a.usedTOTP[key] = now.Add(2 * time.Minute)
	return true
}

func (a *App) authConfigured() bool {
	return !a.accounts.empty()
}
func (a *App) authenticated(r *http.Request) bool {
	c, err := r.Cookie("cortex_session")
	if err != nil {
		return false
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.Expires) {
		if ok {
			delete(a.sessions, c.Value)
		}
		return false
	}
	if s.AccountID != "" {
		if account, exists := a.accounts.account(s.AccountID); !exists || !account.Enabled {
			delete(a.sessions, c.Value)
			return false
		}
	}
	return true
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie("cortex_session")
	if err != nil {
		return ""
	}
	return c.Value
}
func (a *App) newSessionCookie(w http.ResponseWriter, r *http.Request, principal ...string) {
	token := randomToken(32)
	now := time.Now()
	a.authMu.Lock()
	for id, session := range a.sessions {
		if now.After(session.Expires) {
			delete(a.sessions, id)
		}
	}
	for len(a.sessions) >= maxSessions {
		oldestID := ""
		var oldest time.Time
		for id, session := range a.sessions {
			if oldestID == "" || session.Created.Before(oldest) {
				oldestID, oldest = id, session.Created
			}
		}
		delete(a.sessions, oldestID)
	}
	session := sessionInfo{Created: now, Expires: now.Add(7 * 24 * time.Hour), CSRF: randomToken(24)}
	if len(principal) > 0 {
		session.AccountID = principal[0]
	}
	if len(principal) > 1 {
		session.IdentityID = principal[1]
	}
	a.sessions[token] = session
	a.authMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "cortex_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.requestScheme(r) == "https", MaxAge: 7 * 24 * 3600})
}
func (a *App) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("cortex_session"); e == nil {
		a.authMu.Lock()
		delete(a.sessions, c.Value)
		a.authMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "cortex_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.requestScheme(r) == "https"})
}

func directClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
func (a *App) loginAllowed(r *http.Request) bool {
	now, key := time.Now(), a.clientIP(r)
	a.authMu.Lock()
	defer a.authMu.Unlock()
	items := a.loginFailures[key][:0]
	for _, at := range a.loginFailures[key] {
		if now.Sub(at) < loginWindow {
			items = append(items, at)
		}
	}
	if len(items) == 0 {
		delete(a.loginFailures, key)
	} else {
		a.loginFailures[key] = items
	}
	return len(items) < loginAttempts
}
func (a *App) recordLoginFailure(r *http.Request) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if len(a.loginFailures) >= maxLoginClients {
		oldestKey := ""
		var oldest time.Time
		for key, times := range a.loginFailures {
			if len(times) > 0 && (oldestKey == "" || times[len(times)-1].Before(oldest)) {
				oldestKey, oldest = key, times[len(times)-1]
			}
		}
		delete(a.loginFailures, oldestKey)
	}
	key := a.clientIP(r)
	a.loginFailures[key] = append(a.loginFailures[key], time.Now())
}
func (a *App) clearLoginFailures(r *http.Request) {
	a.authMu.Lock()
	delete(a.loginFailures, a.clientIP(r))
	a.authMu.Unlock()
}

func (a *App) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		public := a.publicAPI(r.URL.Path)
		if public {
			if unsafeMethod(r.Method) && !a.sameOrigin(r) {
				http.Error(w, "origin", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !a.authConfigured() {
			http.Error(w, "setup required", http.StatusUnauthorized)
			return
		}
		if !a.authenticated(r) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if unsafeMethod(r.Method) {
			if !a.sameOrigin(r) {
				http.Error(w, "origin", http.StatusForbidden)
				return
			}
			values := r.Header.Values("X-Cortex-CSRF")
			token := sessionToken(r)
			a.authMu.Lock()
			session, ok := a.sessions[token]
			a.authMu.Unlock()
			if len(values) != 1 || !ok || subtle.ConstantTimeCompare([]byte(values[0]), []byte(session.CSRF)) != 1 {
				http.Error(w, "csrf", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func unsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}
func (a *App) authState(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	authed := a.authenticated(r)
	out := map[string]any{"configured": !a.accounts.empty(), "authenticated": authed, "totpEnabled": false, "googleEnabled": x.GoogleEnabled, "googleConfigured": x.GoogleClientID != "" && x.GoogleClientSecret != ""}
	if authed {
		out["googleEmail"] = x.GoogleEmail
		out["googleClientID"] = x.GoogleClientID
		token := sessionToken(r)
		a.authMu.Lock()
		current := a.sessions[token]
		out["csrf"] = current.CSRF
		out["accountId"] = current.AccountID
		out["capabilities"] = a.accounts.capabilities(current.AccountID)
		a.authMu.Unlock()
		if account, found := a.accounts.account(current.AccountID); found {
			for _, identity := range account.Identities {
				if identity.ID == current.IdentityID {
					out["totpEnabled"] = identity.TOTPEnabled
					break
				}
			}
		}
	}
	jsonOut(w, out)
}
func (a *App) authSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	if a.authConfigured() {
		http.Error(w, "already configured", 409)
		return
	}
	var q struct {
		Display  string `json:"display"`
		Username string `json:"username"`
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if !decode(w, r, &q) {
		return
	}
	if len(q.Password) < 7 {
		http.Error(w, "password must be at least 7 characters", 400)
		return
	}
	if q.Password != q.Confirm {
		http.Error(w, "passwords do not match", 400)
		return
	}
	if q.Username == "" {
		q.Username = "admin"
	}
	if q.Display == "" {
		q.Display = q.Username
	}
	account, err := a.accounts.initial(q.Display, q.Username, q.Password)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.newSessionCookie(w, r, account.ID, account.Identities[0].ID)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q struct{ Username, Password, TOTP string }
	if !decode(w, r, &q) {
		return
	}
	if !a.loginAllowed(r) {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	account, identity, ok := a.accounts.authenticate(q.Username, q.Password)
	if !ok {
		a.recordLoginFailure(r)
		http.Error(w, "invalid credentials", 401)
		return
	}
	if identity.TOTPEnabled {
		a.mu.RLock()
		secret := a.settings.Auth.TOTPSecrets[identity.ID]
		a.mu.RUnlock()
		if secret == "" || !a.consumeTOTP(secret, q.TOTP) {
			a.recordLoginFailure(r)
			http.Error(w, "invalid two-factor code", 401)
			return
		}
	}
	a.clearLoginFailures(r)
	a.newSessionCookie(w, r, account.ID, identity.ID)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	a.clearSession(w, r)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if !a.authConfigured() || !a.authenticated(r) {
		http.Error(w, "authentication required", 401)
		return false
	}
	return true
}
func (a *App) authPassword(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q struct{ Current, Password, Confirm string }
	if !decode(w, r, &q) {
		return
	}
	token := sessionToken(r)
	a.authMu.Lock()
	current := a.sessions[token]
	a.authMu.Unlock()
	account, found := a.accounts.account(current.AccountID)
	username := ""
	for _, identity := range account.Identities {
		if identity.ID == current.IdentityID {
			username = identity.Username
		}
	}
	verifiedAccount, verifiedIdentity, verified := a.accounts.authenticate(username, q.Current)
	if !found || !verified || verifiedAccount.ID != current.AccountID || verifiedIdentity.ID != current.IdentityID {
		http.Error(w, "current password is incorrect", 401)
		return
	}
	if len(q.Password) < 7 || q.Password != q.Confirm {
		http.Error(w, "new password must match and be at least 7 characters", 400)
		return
	}
	if err := a.accounts.resetPassword(current.AccountID, q.Password); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.authMu.Lock()
	a.sessions = map[string]sessionInfo{}
	a.authMu.Unlock()
	a.newSessionCookie(w, r, current.AccountID, current.IdentityID)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authTOTPBegin(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	token := sessionToken(r)
	a.authMu.Lock()
	a.pendingTOTP[token] = pendingTOTP{Secret: secret, Expires: time.Now().Add(10 * time.Minute)}
	a.authMu.Unlock()
	jsonOut(w, map[string]string{"secret": secret, "uri": "otpauth://totp/Cortex?secret=" + secret + "&issuer=Cortex"})
}
func (a *App) authTOTPEnable(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	var q struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &q) {
		return
	}
	token := sessionToken(r)
	a.authMu.Lock()
	pending, ok := a.pendingTOTP[token]
	if ok && time.Now().After(pending.Expires) {
		delete(a.pendingTOTP, token)
		ok = false
	}
	a.authMu.Unlock()
	if !ok || !verifyTOTP(pending.Secret, q.Code) {
		http.Error(w, "invalid two-factor code", 400)
		return
	}
	a.authMu.Lock()
	current := a.sessions[token]
	a.authMu.Unlock()
	if err := a.accounts.setIdentityTOTP(current.AccountID, current.IdentityID, true); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a.mu.Lock()
	if a.settings.Auth.TOTPSecrets == nil {
		a.settings.Auth.TOTPSecrets = map[string]string{}
	}
	a.settings.Auth.TOTPSecrets[current.IdentityID] = pending.Secret
	a.mu.Unlock()
	a.authMu.Lock()
	delete(a.pendingTOTP, token)
	a.authMu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authTOTPDisable(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	var q struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &q) {
		return
	}
	token := sessionToken(r)
	a.authMu.Lock()
	current := a.sessions[token]
	a.authMu.Unlock()
	if !a.accounts.verifyIdentityPassword(current.AccountID, current.IdentityID, q.Password) {
		http.Error(w, "password is incorrect", 401)
		return
	}
	if err := a.accounts.setIdentityTOTP(current.AccountID, current.IdentityID, false); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a.mu.Lock()
	delete(a.settings.Auth.TOTPSecrets, current.IdentityID)
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authGoogleConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q struct {
		Enabled                       bool `json:"enabled"`
		ClientID, ClientSecret, Email string
	}
	if !decode(w, r, &q) {
		return
	}
	a.mu.Lock()
	a.settings.Auth.GoogleEnabled = q.Enabled
	a.settings.Auth.GoogleClientID = strings.TrimSpace(q.ClientID)
	if strings.TrimSpace(q.ClientSecret) != "" {
		a.settings.Auth.GoogleClientSecret = strings.TrimSpace(q.ClientSecret)
	}
	a.settings.Auth.GoogleEmail = strings.ToLower(strings.TrimSpace(q.Email))
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) googleStart(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	if !x.GoogleEnabled || x.GoogleClientID == "" || x.GoogleClientSecret == "" {
		http.Error(w, "Google sign-in is not configured", 400)
		return
	}
	state := randomToken(24)
	verifier := randomToken(32)
	hash := sha256.Sum256([]byte(verifier))
	a.authMu.Lock()
	now := time.Now()
	for id, item := range a.oauthStates {
		if now.After(item.Expires) {
			delete(a.oauthStates, id)
		}
	}
	for len(a.oauthStates) >= maxOAuthStates {
		for id := range a.oauthStates {
			delete(a.oauthStates, id)
			break
		}
	}
	scheme := a.requestScheme(r)
	redirect := scheme + "://" + r.Host + "/api/auth/google/callback"
	a.oauthStates[state] = oauthState{Expires: now.Add(10 * time.Minute), Verifier: verifier, Redirect: redirect}
	a.authMu.Unlock()
	q := url.Values{"client_id": {x.GoogleClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {"openid email"}, "state": {state}, "prompt": {"select_account"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode(), 302)
}
func (a *App) googleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	a.authMu.Lock()
	pending, ok := a.oauthStates[state]
	delete(a.oauthStates, state)
	a.authMu.Unlock()
	if !ok || time.Now().After(pending.Expires) {
		http.Error(w, "invalid OAuth state", 400)
		return
	}
	code := r.URL.Query().Get("code")
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	resp, err := http.PostForm("https://oauth2.googleapis.com/token", url.Values{"code": {code}, "client_id": {x.GoogleClientID}, "client_secret": {x.GoogleClientSecret}, "redirect_uri": {pending.Redirect}, "grant_type": {"authorization_code"}, "code_verifier": {pending.Verifier}})
	if err != nil {
		http.Error(w, "Google token exchange failed", 502)
		return
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok) != nil || tok.AccessToken == "" {
		http.Error(w, "Google token exchange failed", 401)
		return
	}
	req, _ := http.NewRequest("GET", "https://openidconnect.googleapis.com/v1/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	ur, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Google user lookup failed", 502)
		return
	}
	defer ur.Body.Close()
	var user struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if ur.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(ur.Body, 1<<20)).Decode(&user) != nil || !user.EmailVerified {
		http.Error(w, "Google account email is not verified", 401)
		return
	}
	if x.GoogleEmail != "" && !strings.EqualFold(x.GoogleEmail, user.Email) {
		http.Error(w, "Google account is not authorized for this Cortex instance", 403)
		return
	}
	a.newSessionCookie(w, r)
	http.Redirect(w, r, "/app/", 302)
}

var _ = os.ErrNotExist
