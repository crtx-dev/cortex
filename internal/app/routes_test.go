package app

import "testing"

func TestEveryAPIRouteHasACompletePolicy(t *testing.T) {
	a := &App{}
	routes := a.apiRoutes()
	if len(routes) != 26 {
		t.Fatalf("route inventory has %d entries, want 26", len(routes))
	}
	seen := map[string]bool{}
	for _, route := range routes {
		p := route.Policy
		if p.Path == "" || p.Path[:5] != "/api/" {
			t.Fatalf("invalid API path %q", p.Path)
		}
		if seen[p.Path] {
			t.Fatalf("duplicate API policy for %s", p.Path)
		}
		seen[p.Path] = true
		if p.Boundary != boundaryPublic && p.Boundary != boundarySession {
			t.Fatalf("route %s has unknown boundary %q", p.Path, p.Boundary)
		}
		if len(p.Methods) == 0 || route.Handler == nil {
			t.Fatalf("route %s has incomplete method/handler policy", p.Path)
		}
	}
}

func TestOnlyDeliberateRoutesArePublic(t *testing.T) {
	a := &App{}
	want := map[string]bool{
		"/api/launcher/instances": true,
		"/api/auth/state":         true, "/api/auth/setup": true, "/api/auth/login": true,
		"/api/auth/google/start": true, "/api/auth/google/callback": true,
		"/api/health": true,
	}
	for _, route := range a.apiRoutes() {
		if got := route.Policy.Boundary == boundaryPublic; got != want[route.Policy.Path] {
			t.Fatalf("public classification for %s = %v", route.Policy.Path, got)
		}
	}
}
