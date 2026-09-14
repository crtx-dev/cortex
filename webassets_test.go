package cortex

import (
	"io/fs"
	"strings"
	"testing"
)

func TestGeneratedFrontendEmbedded(t *testing.T) {
	frontend := PublicFS()
	for _, name := range []string{"index.html", "assets/css/style.css", "assets/js/script.js"} {
		if _, err := fs.Stat(frontend, name); err != nil {
			t.Fatalf("generated frontend %q is not embedded: %v", name, err)
		}
	}
	b, err := fs.ReadFile(frontend, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if !strings.Contains(html, "assets/css/style.css") || !strings.Contains(html, "assets/js/script.js") {
		t.Fatal("generated index does not reference Nift-tracked frontend assets")
	}
}

// TestApplicationNavigationHasNoCrossSiteLink is a regression test for a
// dogfooding-discovered defect: a request to add Gantry to the public-facing
// websites leaked a hard-coded https://gantry.cv link into the authenticated
// application header/navigation. The application menu must only carry
// application routes, never unsolicited cross-site branding.
func TestApplicationNavigationHasNoCrossSiteLink(t *testing.T) {
	b, err := fs.ReadFile(PublicFS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "gantry.cv") {
		t.Fatal("application frontend contains a hard-coded gantry.cv cross-site link; remove it from the Nift source and regenerate")
	}
}
