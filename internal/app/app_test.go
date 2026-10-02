package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveConfinesRoot(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.resolve("../escape"); err == nil {
		t.Fatal("expected escape rejection")
	}
}
func TestSettingsPersistWithoutPublicKey(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	a.settings.Keys["opencode"] = "secret"
	if err = a.saveSettings(); err != nil {
		t.Fatal(err)
	}
	p := a.publicSettings()
	for _, x := range p.Providers {
		if _, ok := x["key"]; ok {
			t.Fatal("public settings leaked key")
		}
	}
	b, _ := os.ReadFile(a.settingsPath())
	if len(b) == 0 {
		t.Fatal("settings not written")
	}
}
func TestAssistantTexts(t *testing.T) {
	v := map[string]any{"messages": []any{map[string]any{"info": map[string]any{"role": "assistant"}, "parts": []any{map[string]any{"type": "text", "text": "hello"}}}}}
	got := assistantTexts(v)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("%#v", got)
	}
}

func TestCollectUsageFromOpenCodeSessionTokens(t *testing.T) {
	v := map[string]any{
		"type":   "step-finish",
		"tokens": map[string]any{"input": float64(1234), "output": float64(321), "reasoning": float64(42)},
		"cost":   float64(0.0123),
	}
	var input, output uint64
	var cost float64
	collectUsage(v, &input, &output, &cost)
	if input != 1234 || output != 321 {
		t.Fatalf("usage = %d/%d", input, output)
	}
	if cost != 0.0123 {
		t.Fatalf("cost = %v", cost)
	}
}

func TestProviderRuntimeConfigUsesSelectedModel(t *testing.T) {
	p, ok := providerByID("openai")
	if !ok {
		t.Fatal("openai provider missing")
	}
	b, err := cortexOpenCodeConfig(p, "gpt-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "openai/gpt-5.2" {
		t.Fatalf("model = %#v", cfg["model"])
	}
	providers, _ := cfg["provider"].(map[string]any)
	openai, _ := providers["openai"].(map[string]any)
	options, _ := openai["options"].(map[string]any)
	if options["apiKey"] != "{env:CORTEX_PROVIDER_API_KEY}" {
		t.Fatalf("api key config = %#v", options["apiKey"])
	}
}

func TestSubscriptionProviderDoesNotEmbedAPIKeyConfig(t *testing.T) {
	p, ok := providerByID("github-copilot")
	if !ok {
		t.Fatal("github copilot provider missing")
	}
	b, err := cortexOpenCodeConfig(p, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("CORTEX_PROVIDER_API_KEY")) {
		t.Fatal("OAuth-backed provider unexpectedly embeds API-key config")
	}
}

func TestAgentRunArgsSeparateFilesFromPrompt(t *testing.T) {
	args := agentRunArgs("/work", "openai/gpt-5.2", "sess1", []string{"/tmp/a.png", "/tmp/b.png"}, "can you describe what this image is to me")
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatal("missing -- separator")
	}
	if strings.Join(args[sep+1:], " ") != "can you describe what this image is to me" {
		t.Fatalf("prompt = %v", args[sep+1:])
	}
	files := []string{}
	for i, a := range args {
		if a == "--file" && i+1 < sep {
			files = append(files, args[i+1])
		}
	}
	if strings.Join(files, ",") != "/tmp/a.png,/tmp/b.png" {
		t.Fatalf("files = %v", files)
	}
	foundSession := false
	for _, a := range args[:sep] {
		if a == "sess1" {
			foundSession = true
		}
	}
	if !foundSession {
		t.Fatal("session flag lost before separator")
	}
}

func TestAgentRunArgsNoImagesStillSeparatePrompt(t *testing.T) {
	args := agentRunArgs("/work", "openai/gpt-5.2", "", nil, "hello world")
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || strings.Join(args[sep+1:], " ") != "hello world" {
		t.Fatalf("args = %v", args)
	}
}

func TestWriteImageAttachmentPersistsDecodedImage(t *testing.T) {
	dir := t.TempDir()
	raw := []byte{0x89, 0x50, 0x4e, 0x47}
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	path, err := writeImageAttachment(dir, imageUpload{Name: "shot.png", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".png") {
		t.Fatalf("extension = %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("decoded image mismatch")
	}
}

func TestWriteImageAttachmentRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, maxImageBytes+1))
	if _, err := writeImageAttachment(dir, imageUpload{Name: "big.png", Data: data}); err == nil {
		t.Fatal("oversize image accepted")
	}
}

func TestWriteImageAttachmentRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	cases := []imageUpload{
		{Name: "a.png", Data: "data:image/svg+xml;base64,PHN2Zz4="},
		{Name: "b.bin", Data: "data:application/octet-stream;base64,AAAA"},
		{Name: "c.png", Data: "not a data url"},
		{Name: "d.png", Data: "data:image/png;base64,%%%"},
	}
	for _, c := range cases {
		if _, err := writeImageAttachment(dir, c); err == nil {
			t.Fatalf("accepted %#v", c)
		}
	}
}

func TestAgentRunRejectsTooManyImages(t *testing.T) {
	a := hardeningTestApp(t)
	var images []imageUpload
	for i := 0; i < maxImageCount+1; i++ {
		images = append(images, imageUpload{Name: "x.png", Data: "data:image/png;base64,AAAA"})
	}
	payload := map[string]any{"workspace": a.root, "prompt": "look", "clientSession": "sess1", "images": images}
	b, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "/api/agent/run", bytes.NewReader(b))
	w := httptest.NewRecorder()
	a.agentRun(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestRewriteImageURLs(t *testing.T) {
	ev := map[string]any{"type": "message.part.updated", "part": map[string]any{"type": "file", "mediaType": "image/png", "url": "file:///tmp/x/shot.png", "filename": "shot.png"}}
	rewriteImageURLs(ev, "sess1")
	u := ev["part"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(u, "/api/agent/image?session=sess1&path=") {
		t.Fatalf("url = %s", u)
	}
	nonImage := map[string]any{"part": map[string]any{"type": "file", "mediaType": "text/plain", "url": "file:///tmp/a.txt"}}
	rewriteImageURLs(nonImage, "sess1")
	if nonImage["part"].(map[string]any)["url"] != "file:///tmp/a.txt" {
		t.Fatal("non-image url rewritten")
	}
	dataImage := map[string]any{"part": map[string]any{"type": "file", "mediaType": "image/png", "url": "data:image/png;base64,AAAA"}}
	rewriteImageURLs(dataImage, "sess1")
	if dataImage["part"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
		t.Fatal("data url rewritten")
	}
	tool := map[string]any{"part": map[string]any{"type": "tool", "state": map[string]any{"attachments": []any{map[string]any{"mime": "image/webp", "url": "file:///tmp/x/a.webp"}}}}}
	rewriteImageURLs(tool, "sess1")
	atts := tool["part"].(map[string]any)["state"].(map[string]any)["attachments"].([]any)
	if !strings.HasPrefix(atts[0].(map[string]any)["url"].(string), "/api/agent/image?") {
		t.Fatalf("attachment url = %v", atts[0])
	}
}

func TestAgentImageServesOnlySessionImages(t *testing.T) {
	a := hardeningTestApp(t)
	dir := filepath.Join(a.dataDir, "sessions", "sess1", "data")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if err := os.WriteFile(filepath.Join(dir, "img.png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/agent/image?session=sess1&path="+url.QueryEscape(path), nil)
		w := httptest.NewRecorder()
		a.agentImage(w, r)
		return w
	}
	if w := get(filepath.Join(dir, "img.png")); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("valid image: code=%d ct=%q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get("/etc/passwd"); w.Code == 200 {
		t.Fatal("escaping path served")
	}
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := get(filepath.Join(dir, "x.txt")); w.Code == 200 {
		t.Fatal("non-image served")
	}
	if w := get(filepath.Join(dir, "missing.png")); w.Code != 404 {
		t.Fatalf("missing file: code=%d", w.Code)
	}
}

func TestAssistantImagesRecoveredFromExport(t *testing.T) {
	v := map[string]any{"messages": []any{
		map[string]any{"info": map[string]any{"role": "user"}, "parts": []any{map[string]any{"type": "file", "mediaType": "image/png", "url": "file:///u/a.png", "filename": "a.png"}}},
		map[string]any{"info": map[string]any{"role": "assistant"}, "parts": []any{
			map[string]any{"type": "file", "mediaType": "image/png", "url": "file:///g/x.png", "filename": "x.png"},
			map[string]any{"type": "file", "mediaType": "text/plain", "url": "file:///g/t.txt"},
			map[string]any{"type": "tool", "state": map[string]any{"attachments": []any{map[string]any{"mime": "image/webp", "url": "file:///g/a.webp"}}}},
		}},
	}}
	rewriteImageURLs(v, "sess1")
	imgs := assistantImages(v)
	if len(imgs) != 2 {
		t.Fatalf("images = %v", imgs)
	}
	for _, im := range imgs {
		if !strings.HasPrefix(im["url"], "/api/agent/image?session=sess1&path=") {
			t.Fatalf("url not rewritten: %v", im)
		}
	}
	if imgs[0]["name"] != "x.png" {
		t.Fatalf("filename lost: %v", imgs[0])
	}
}

func TestParseModelLines(t *testing.T) {
	out := []byte("opencode/deepseek-v4-flash\nopencode/deepseek-v4-pro\n\nopencode/deepseek-v4-flash\nother/gpt\n")
	models := parseModelLines(out, "opencode")
	if len(models) != 2 || models[0] != "deepseek-v4-flash" || models[1] != "deepseek-v4-pro" {
		t.Fatalf("models = %v", models)
	}
}

func TestZenConfigIncludesCatalogueAndCustomModel(t *testing.T) {
	p, ok := providerByID("opencode")
	if !ok {
		t.Fatal("opencode provider missing")
	}
	b, err := cortexOpenCodeConfig(p, "future-custom-model")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "opencode-chat/future-custom-model" {
		t.Fatalf("model = %#v", cfg["model"])
	}
	providers, _ := cfg["provider"].(map[string]any)
	if len(providers) != 4 {
		t.Fatalf("provider entries = %d, want 4", len(providers))
	}
	total := 0
	for _, v := range providers {
		entry, _ := v.(map[string]any)
		if entry["npm"] == "" || entry["name"] != "OpenCode Zen" {
			t.Fatalf("entry = %#v", entry)
		}
		opts, _ := entry["options"].(map[string]any)
		if opts["apiKey"] != "{env:CORTEX_PROVIDER_API_KEY}" || opts["baseURL"] != "https://opencode.ai/zen/v1" {
			t.Fatalf("options = %#v", opts)
		}
		models, _ := entry["models"].(map[string]any)
		total += len(models)
	}
	if total != len(zenModels)+1 {
		t.Fatalf("model total = %d, want %d", total, len(zenModels)+1)
	}
}

func TestZenModelFamilyRouting(t *testing.T) {
	cases := map[string]string{
		"grok-4.6": "opencode", "gpt-5.6-sol": "opencode", "muse-spark-1.2": "opencode",
		"claude-sonnet-5": "opencode-anthropic", "claude-haiku-4-5": "opencode-anthropic",
		"gemini-3.5-flash": "opencode-google", "gemini-3-flash": "opencode-google",
		"deepseek-v4-flash": "opencode-chat", "glm-5": "opencode-chat", "kimi-k3": "opencode-chat",
		"laguna-s-2.1-free": "opencode-chat", "future-custom": "opencode-chat",
	}
	p, _ := providerByID("opencode")
	for model, want := range cases {
		if got := zenProviderKey(model); got != want {
			t.Fatalf("%s -> %s, want %s", model, got, want)
		}
		if ref := modelRefFor(p, model); ref != want+"/"+model {
			t.Fatalf("%s ref = %s", model, ref)
		}
	}
}

func TestAgentModelsZenUsesHardcodedCatalogue(t *testing.T) {
	a := hardeningTestApp(t)
	r := httptest.NewRequest("GET", "/api/agent/models?provider=opencode", nil)
	w := httptest.NewRecorder()
	a.agentModels(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var out struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != len(zenModels) {
		t.Fatalf("models = %d, want %d", len(out.Models), len(zenModels))
	}
}

func TestOpenCodeGoConfigUsesNativeProvider(t *testing.T) {
	p, ok := providerByID("opencode-go")
	if !ok {
		t.Fatal("opencode-go provider missing")
	}
	if p.Label != "OpenCode Go" || p.OpenCodeID != "opencode-go" || p.DefaultModel != "deepseek-v4-flash" {
		t.Fatalf("provider = %#v", p)
	}
	b, err := cortexOpenCodeConfig(p, "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "opencode-go/deepseek-v4-flash" {
		t.Fatalf("model = %#v", cfg["model"])
	}
	providers, _ := cfg["provider"].(map[string]any)
	entry, _ := providers["opencode-go"].(map[string]any)
	opts, _ := entry["options"].(map[string]any)
	if opts["apiKey"] != "{env:CORTEX_PROVIDER_API_KEY}" {
		t.Fatalf("options = %#v", opts)
	}
	if _, ok := opts["baseURL"]; ok {
		t.Fatalf("OpenCode Go should use OpenCode's native provider routing, got %#v", opts)
	}
}

func TestAgentModelsGoUsesHardcodedCatalogue(t *testing.T) {
	a := hardeningTestApp(t)
	r := httptest.NewRequest("GET", "/api/agent/models?provider=opencode-go", nil)
	w := httptest.NewRecorder()
	a.agentModels(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var out struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != len(goModels) {
		t.Fatalf("models = %d, want %d", len(out.Models), len(goModels))
	}
	found := false
	for _, model := range out.Models {
		if model == "deepseek-v4-flash" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("deepseek-v4-flash missing from OpenCode Go catalogue")
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	h := passwordHash("correct horse battery staple")
	if !verifyPassword(h, "correct horse battery staple") {
		t.Fatal("password did not verify")
	}
	if verifyPassword(h, "wrong password") {
		t.Fatal("wrong password verified")
	}
}

func TestTOTPVerification(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	code := totpCode(secret, time.Now())
	if !verifyTOTP(secret, code) {
		t.Fatal("current TOTP did not verify")
	}
	if verifyTOTP(secret, "000000") && code != "000000" {
		t.Fatal("invalid TOTP verified")
	}
}

func TestRequestSchemeTrustsLoopbackProxy(t *testing.T) {
	a := &App{trustProxy: true}
	r, _ := http.NewRequest("GET", "http://cortex.example.com/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := a.requestScheme(r); got != "https" {
		t.Fatalf("requestScheme = %q, want https", got)
	}
}

func TestRequestSchemeRejectsForwardedProtoFromRemoteClient(t *testing.T) {
	a := &App{trustProxy: true}
	r, _ := http.NewRequest("GET", "http://cortex.example.com/", nil)
	r.RemoteAddr = "203.0.113.10:54321"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := a.requestScheme(r); got != "http" {
		t.Fatalf("requestScheme = %q, want http", got)
	}
}
