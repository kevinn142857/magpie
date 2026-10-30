package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/tokenpack"
)

// quotaPanelHome isolates configuration and the shell's process-wide mode.
func quotaPanelHome(t *testing.T) {
	t.Helper()
	onePortHome(t)
	t.Setenv("MAGPIE_QUOTA_PANEL", "1")
	previous := webPage.Load()
	t.Cleanup(func() { webPage.Store(previous) })
}

func TestQuotaPanelRoutesAreFocused(t *testing.T) {
	quotaPanelHome(t)
	h := Handler(webHost{}, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/quota/app.js") {
		t.Fatalf("quota panel entry: status %d; dedicated console missing", w.Code)
	}
	for _, target := range []string{"/api/settings", "/api/providers", "/api/plugins", "/app.js"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("unrelated route %s returned %d, want 404", target, w.Code)
		}
	}
}

func TestTokenPackLogsShowSettledRequestsAcrossPackages(t *testing.T) {
	quotaPanelHome(t)
	p, err := tokenpack.Create("Starter", []string{"test/model"}, 1000000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := tokenpack.Reserve(p.ID, "test/model", "/v1/chat/completions", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokenpack.Complete(reservation.ID, 123, 200, true); err != nil {
		t.Fatal(err)
	}
	h := Handler(webHost{}, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/token-packs/logs", nil))
	var result struct {
		Logs []struct {
			PackName string `json:"packName"`
			Tokens   int64  `json:"tokens"`
			Outcome  string `json:"outcome"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(result.Logs) != 1 || result.Logs[0].Outcome != "completed" || result.Logs[0].PackName != "Starter" || result.Logs[0].Tokens != 123 {
		t.Fatalf("all-package logs: %d %s", w.Code, w.Body.String())
	}
}

func TestTokenPackRejectsCrossOriginMutation(t *testing.T) {
	quotaPanelHome(t)
	h := Handler(webHost{}, nil)
	for _, origin := range []string{"null", "https://attacker.example", "file://local"} {
		r := httptest.NewRequest("POST", "http://relay.example/api/token-packs", strings.NewReader(`{}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Origin %q returned %d, want 403", origin, w.Code)
		}
	}
}

func TestTokenPackManagementAndShare(t *testing.T) {
	quotaPanelHome(t)
	t.Setenv("MAGPIE_PUBLIC_URL", "https://relay.example")
	const administrator = "administrator-secret-0123456789"
	h := webServed("3430", administrator, 0, Handler(webHost{}, nil))
	call := func(method, target, body string, admin bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.RemoteAddr = "10.0.0.5:50001"
		r.Header.Set("Content-Type", "application/json")
		if admin {
			r.AddCookie(&http.Cookie{Name: "magpie_web_3430", Value: administrator})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/api/token-packs", "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unprivileged list: %d", w.Code)
	}
	w := call("POST", "/api/token-packs", `{"name":"<script>alert(1)</script>","models":["test/model"],"totalTokens":1000000}`, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create package: %d %s", w.Code, w.Body.String())
	}
	if got := w.Result().Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("created package Content-Type %q, want JSON", got)
	}
	var created struct {
		Pack struct {
			ID        string `json:"id"`
			ShareURL  string `json:"shareURL"`
			APIKey    string `json:"apiKey"`
			Remaining int64  `json:"remainingTokens"`
		} `json:"pack"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	p := created.Pack
	if p.ID == "" || p.Remaining != 1000000 || !strings.HasPrefix(p.ShareURL, "https://relay.example/share/") || p.APIKey != "" {
		t.Fatalf("unexpected package response: %s", w.Body.String())
	}
	sharePath := strings.TrimPrefix(p.ShareURL, "https://relay.example")
	w = call("GET", sharePath, "", false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "sk-magpie-key-") || !strings.Contains(w.Body.String(), "https://relay.example/v1") {
		t.Fatalf("public package config: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<script>alert(1)</script>") || strings.Contains(w.Body.String(), administrator) {
		t.Fatal("public package page rendered unsafe or administrative content")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("share page can leak credentials through caching or referrers")
	}
	w = call("PATCH", "/api/token-packs/"+p.ID, `{"enabled":false}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("disable package: %d %s", w.Code, w.Body.String())
	}
	w = call("GET", sharePath, "", false)
	if w.Code != http.StatusGone || strings.Contains(w.Body.String(), "sk-magpie-key-") {
		t.Fatalf("disabled share exposed config: %d", w.Code)
	}
}

func TestTokenPackPayloadAndConfiguredDomain(t *testing.T) {
	quotaPanelHome(t)
	h := Handler(webHost{}, nil)
	create := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/token-packs", strings.NewReader(body))
		r.Host = "untrusted.example"
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(w, r)
		return w
	}
	valid := `{"name":"Starter","models":["test/model"],"totalTokens":1000000}`
	if w := create(valid); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing configured domain: %d %s", w.Code, w.Body.String())
	}
	t.Setenv("MAGPIE_PUBLIC_URL", "https://relay.example")
	for _, body := range []string{valid + `{}`, `{"name":"x","models":[],"totalTokens":1}`, `{"name":"x","models":["test/model"],"totalTokens":0}`, `{"name":"x","models":["test/model"],"totalTokens":1,"unexpected":true}`} {
		if w := create(body); w.Code != http.StatusBadRequest {
			t.Errorf("invalid payload returned %d: %s", w.Code, w.Body.String())
		}
	}
}

func TestTokenPackShareDoesNotUseAdministratorLogin(t *testing.T) {
	quotaPanelHome(t)
	h := webServed("3430", "administrator-secret-0123456789", 0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid public share reached the administrator page")
	}))
	r := httptest.NewRequest(http.MethodGet, "/share/not-a-valid-package", nil)
	r.RemoteAddr = "10.0.0.5:50001"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("invalid share: status %d, want 404 without administrator login", w.Code)
	}
	if len(w.Result().Cookies()) > 0 || strings.Contains(w.Body.String(), "administrator-secret") {
		t.Fatal("share response exposed administrator credentials")
	}
}
