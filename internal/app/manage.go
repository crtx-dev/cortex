package app

import (
	"net/http"

	coreauth "github.com/gantry-tools/gantry-core/auth"
)

func (a *App) managementAllowed(r *http.Request, capability string) bool {
	if !a.authenticated(r) {
		return false
	}
	a.authMu.Lock()
	session := a.sessions[sessionToken(r)]
	a.authMu.Unlock()
	return coreauth.HasCapability(a.accounts.capabilities(session.AccountID), capability)
}

func (a *App) manageRoot(static http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manage/" {
			http.NotFound(w, r)
			return
		}
		if !a.authenticated(r) {
			http.Redirect(w, r, "/app/", http.StatusFound)
			return
		}
		if !a.managementAllowed(r, "accounts.manage") && !a.managementAllowed(r, "roles.manage") && !a.managementAllowed(r, "launcher.configure.all") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = "/manage.html"
		static.ServeHTTP(w, clone)
	}
}

func (a *App) manageUsers(w http.ResponseWriter, r *http.Request) {
	if !a.managementAllowed(r, "accounts.manage") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		jsonOut(w, map[string]any{"accounts": a.accounts.list(), "roles": a.accounts.roleList()})
		return
	}
	var input struct {
		Action, ID, Display, Username, Password string
		Enabled                                 bool
		Roles                                   []string
	}
	if !decode(w, r, &input) {
		return
	}
	var err error
	switch input.Action {
	case "create-account":
		_, err = a.accounts.create(input.Display, input.Username, input.Password, input.Roles)
	case "update-account":
		err = a.accounts.update(input.ID, input.Display, input.Enabled, input.Roles)
	case "reset-password":
		err = a.accounts.resetPassword(input.ID, input.Password)
	default:
		http.Error(w, "unsupported action", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}

func (a *App) manageRoles(w http.ResponseWriter, r *http.Request) {
	if !a.managementAllowed(r, "roles.manage") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	jsonOut(w, map[string]any{"roles": a.accounts.roleList(), "capabilities": cortexCapabilities})
}
