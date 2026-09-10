package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLauncherRootCardinalityAndConfigPage(t *testing.T) {
	a := hardeningTestApp(t)
	h := a.Handler()
	request := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil))
		return w
	}
	if got := request("/"); got.Code != http.StatusFound || got.Header().Get("Location") != "/app/" {
		t.Fatalf("zero instances: %d %q", got.Code, got.Header().Get("Location"))
	}
	if _, err := a.db.Exec("INSERT INTO launcher_instances(id,position,name,domain,port) VALUES('one',0,'Local','localhost',7331)"); err != nil {
		t.Fatal(err)
	}
	if got := request("/"); got.Code != http.StatusFound || got.Header().Get("Location") != "http://localhost:7331/app/" {
		t.Fatalf("one instance: %d %q", got.Code, got.Header().Get("Location"))
	}
	if _, err := a.db.Exec("INSERT INTO launcher_instances(id,position,name,domain) VALUES('two',1,'Hosted','cortex.example')"); err != nil {
		t.Fatal(err)
	}
	if got := request("/"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "Cortex instances") {
		t.Fatalf("multiple instances: %d %q", got.Code, got.Body.String())
	}
	if got := request("/?config"); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), "Authentication required") || strings.Contains(got.Body.String(), "Current instances") {
		t.Fatalf("config page: %d %q", got.Code, got.Body.String())
	}
}

func TestApplicationMovedToApp(t *testing.T) {
	a := hardeningTestApp(t)
	for _, path := range []string{"/app/", "/assets/css/style.css", "/assets/css/launcher.css", "/assets/js/launcher.js"} {
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, w.Code)
		}
	}
}
