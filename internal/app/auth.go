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
	"strconv"
	"strings"
	"time"
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
	if a.publicOrigin != nil {
		return strings.EqualFold(hostport, a.publicOrigin.Host)
	}
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || func() bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }()
}

func (a *App) sameOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
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
	PasswordHash       string `json:"passwordHash,omitempty"`
	TOTPSecret         string `json:"totpSecret,omitempty"`
	TOTPEnabled        bool   `json:"totpEnabled,omitempty"`
	GoogleEnabled      bool   `json:"googleEnabled,omitempty"`
	GoogleClientID     string `json:"googleClientId,omitempty"`
	GoogleClientSecret string `json:"googleClientSecret,omitempty"`
	GoogleEmail        string `json:"googleEmail,omitempty"`
}
type sessionInfo struct {
	Created time.Time
	Expires time.Time
	CSRF    string
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
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	iter := 310000
	dk := pbkdf2SHA256([]byte(password), salt, iter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk))
}
func verifyPassword(stored, password string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 100000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(p[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(p[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
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
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings.Auth.PasswordHash != ""
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
	return true
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie("cortex_session")
	if err != nil {
		return ""
	}
	return c.Value
}
func (a *App) newSessionCookie(w http.ResponseWriter, r *http.Request) {
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
	a.sessions[token] = sessionInfo{Created: now, Expires: now.Add(7 * 24 * time.Hour), CSRF: randomToken(24)}
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
	out := map[string]any{"configured": x.PasswordHash != "", "authenticated": authed, "totpEnabled": x.TOTPEnabled, "googleEnabled": x.GoogleEnabled, "googleConfigured": x.GoogleClientID != "" && x.GoogleClientSecret != ""}
	if authed {
		out["googleEmail"] = x.GoogleEmail
		out["googleClientID"] = x.GoogleClientID
		token := sessionToken(r)
		a.authMu.Lock()
		out["csrf"] = a.sessions[token].CSRF
		a.authMu.Unlock()
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
	a.mu.Lock()
	a.settings.Auth.PasswordHash = passwordHash(q.Password)
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.newSessionCookie(w, r)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q struct{ Password, TOTP string }
	if !decode(w, r, &q) {
		return
	}
	if !a.loginAllowed(r) {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	if !verifyPassword(x.PasswordHash, q.Password) {
		a.recordLoginFailure(r)
		http.Error(w, "invalid credentials", 401)
		return
	}
	if x.TOTPEnabled && !a.consumeTOTP(x.TOTPSecret, q.TOTP) {
		a.recordLoginFailure(r)
		http.Error(w, "invalid two-factor code", 401)
		return
	}
	a.clearLoginFailures(r)
	a.newSessionCookie(w, r)
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
	a.mu.RLock()
	old := a.settings.Auth.PasswordHash
	a.mu.RUnlock()
	if !verifyPassword(old, q.Current) {
		http.Error(w, "current password is incorrect", 401)
		return
	}
	if len(q.Password) < 7 || q.Password != q.Confirm {
		http.Error(w, "new password must match and be at least 7 characters", 400)
		return
	}
	a.mu.Lock()
	a.settings.Auth.PasswordHash = passwordHash(q.Password)
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.authMu.Lock()
	a.sessions = map[string]sessionInfo{}
	a.authMu.Unlock()
	a.newSessionCookie(w, r)
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
	a.mu.Lock()
	a.settings.Auth.TOTPSecret = pending.Secret
	a.settings.Auth.TOTPEnabled = true
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
	a.mu.RLock()
	hash := a.settings.Auth.PasswordHash
	a.mu.RUnlock()
	if !verifyPassword(hash, q.Password) {
		http.Error(w, "password is incorrect", 401)
		return
	}
	a.mu.Lock()
	a.settings.Auth.TOTPEnabled = false
	a.settings.Auth.TOTPSecret = ""
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
func (a *App) authDebugHash() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	h := sha256.Sum256([]byte(a.settings.Auth.PasswordHash))
	return hex.EncodeToString(h[:4])
}

var _ = os.ErrNotExist
