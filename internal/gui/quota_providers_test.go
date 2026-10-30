package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

type quotaProviderRoundTripFunc func(*http.Request) (*http.Response, error)

func (f quotaProviderRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQuotaProviderSelectedClientReachesVendorListBranches(t *testing.T) {
	quotaPanelHome(t)
	for _, vendor := range []string{"kilo", "clinepass", "public"} {
		t.Run(vendor, func(t *testing.T) {
			body := `{"data":[{"id":"m1"}]}`
			if vendor == "clinepass" {
				body = `{"clinePass":[{"id":"m1"}]}`
			}
			if vendor == "public" {
				body = `{"data":[{"model_id":"gpt-image-2","output_modalities":"image"}]}`
			}
			// A bypass reaches only this local fixture, never a real vendor.
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer up.Close()
			var requests []string
			client := &http.Client{Transport: quotaProviderRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.URL.Path)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			ctx := catalog.WithHTTPClient(context.Background(), client)
			if vendor == "public" {
				old := catalog.PublicCatalogs
				catalog.PublicCatalogs = map[string]string{"127.0.0.1": up.URL + "/public/models"}
				defer func() { catalog.PublicCatalogs = old }()
				catalog.PublicDrawers(ctx, up.URL+"/v1")
			} else {
				p := provider.Provider{Preset: vendor, Chat: up.URL + "/v1", Key: "vendor-key-123"}
				if _, err := p.List(ctx); err != nil {
					t.Fatalf("selected client was bypassed: %v", err)
				}
			}
			if len(requests) == 0 || (vendor == "clinepass" && !strings.Contains(requests[0], "recommended-models")) {
				t.Fatalf("vendor list bypassed selected client: %v", requests)
			}
		})
	}
}

func quotaProviderTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	quotaPanelHome(t)
	mux := http.NewServeMux()
	quotaProviderRoutes(mux)
	return mux
}

func quotaProviderCall(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func quotaProviderCreate(t *testing.T, h http.Handler, base, secret string) string {
	t.Helper()
	w := quotaProviderCall(t, h, "POST", "/api/quota-providers", map[string]any{"name": "Example", "protocol": "chat", "baseURL": base, "apiKey": secret, "models": []string{"m1"}})
	if w.Code != 201 {
		t.Fatalf("create provider: %d %s", w.Code, w.Body)
	}
	var out struct {
		Provider struct {
			ID string `json:"id"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Provider.ID == "" {
		t.Fatal("provider has no ID")
	}
	return out.Provider.ID
}

func TestQuotaProviderCreateListEditAndToggleWithoutSecrets(t *testing.T) {
	mux := quotaProviderTestMux(t)
	const secret = "provider-secret-to-preserve-123"
	id := quotaProviderCreate(t, mux, "https://api.example.invalid", secret)
	list := quotaProviderCall(t, mux, "GET", "/api/quota-providers", nil)
	if list.Code != 200 || strings.Contains(list.Body.String(), secret) {
		t.Fatalf("unsafe provider list: %d %s", list.Code, list.Body)
	}
	var result struct {
		Providers []struct {
			ID, Name, Protocol, BaseURL, KeyMasked string
			Models                                 []string
			Enabled, KeyConfigured, Editable       bool
		}
	}
	if err := json.Unmarshal(list.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 1 || result.Providers[0].BaseURL != "https://api.example.invalid/v1" || !result.Providers[0].KeyConfigured || !result.Providers[0].Editable {
		t.Fatalf("provider summary: %s", list.Body)
	}
	for _, patch := range []map[string]any{{"name": "Renamed", "apiKey": ""}, {"enabled": false}, {"models": []string{"m2", "m3"}}, {"enabled": true}} {
		w := quotaProviderCall(t, mux, "PATCH", "/api/quota-providers/"+id, patch)
		if w.Code != 200 {
			t.Fatalf("edit provider: %d %s", w.Code, w.Body)
		}
	}
	stored, err := provider.Stored()
	if err != nil || len(stored) != 1 || stored[0].Key != secret || stored[0].Name != "Renamed" || stored[0].Off || len(stored[0].Models) != 2 {
		t.Fatalf("stored state not preserved: %+v %v", stored, err)
	}
}

func TestQuotaProviderChangedEndpointCannotReuseSavedKey(t *testing.T) {
	mux := quotaProviderTestMux(t)
	var asked atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	defer up.Close()
	id := quotaProviderCreate(t, mux, up.URL+"/first", "saved-provider-secret-123")
	for _, route := range []string{"/models", "/probe"} {
		for _, change := range []map[string]any{{"baseURL": up.URL + "/different"}, {"protocol": "responses"}} {
			form := map[string]any{"id": id, "models": []string{"m1"}, "apiKey": ""}
			for k, v := range change {
				form[k] = v
			}
			w := quotaProviderCall(t, mux, "POST", "/api/quota-providers"+route, form)
			if w.Code != 400 {
				t.Errorf("changed endpoint %s reused key: %d %s", route, w.Code, w.Body)
			}
		}
	}
	w := quotaProviderCall(t, mux, "PATCH", "/api/quota-providers/"+id, map[string]any{"baseURL": up.URL + "/different", "apiKey": ""})
	if w.Code != 400 || asked.Load() != 0 {
		t.Fatalf("endpoint guard: status=%d requests=%d", w.Code, asked.Load())
	}
}

func TestQuotaProviderDraftOperationsReuseOnlySameEndpointKey(t *testing.T) {
	mux := quotaProviderTestMux(t)
	const secret = "saved-probe-secret-123456"
	var wrong atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			wrong.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	id := quotaProviderCreate(t, mux, up.URL+"/v1", secret)
	for _, route := range []string{"/models", "/probe"} {
		w := quotaProviderCall(t, mux, "POST", "/api/quota-providers"+route, map[string]any{"id": id, "models": []string{"m1"}})
		if w.Code != 200 || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("draft %s: %d %s", route, w.Code, w.Body)
		}
		if route == "/models" && !strings.Contains(w.Body.String(), `"m2"`) {
			t.Fatalf("model listing: %s", w.Body)
		}
		if route == "/probe" && !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("probe: %s", w.Body)
		}
	}
	if wrong.Load() {
		t.Fatal("saved key was not used on unchanged endpoint")
	}
}

func TestQuotaProviderChangedEndpointUsesOnlyExplicitNewKey(t *testing.T) {
	mux := quotaProviderTestMux(t)
	const oldKey, newKey = "old-provider-secret-123", "new-provider-secret-456"
	var wrong atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+newKey || strings.Contains(fmt.Sprint(r.Header), oldKey) {
			wrong.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	defer up.Close()
	id := quotaProviderCreate(t, mux, "https://old.example.invalid/v1", oldKey)
	w := quotaProviderCall(t, mux, "POST", "/api/quota-providers/models", map[string]any{"id": id, "baseURL": up.URL + "/v1", "apiKey": newKey})
	if w.Code != 200 || wrong.Load() {
		t.Fatalf("new-key isolation: %d %s wrong=%v", w.Code, w.Body, wrong.Load())
	}
	stored, _ := provider.Stored()
	if stored[0].Key != oldKey || stored[0].Chat != "https://old.example.invalid/v1" {
		t.Fatal("draft fetch saved provider edits")
	}
}

func TestQuotaProviderRedirectsNeverForwardCredentials(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			mux := quotaProviderTestMux(t)
			var stolen atomic.Int64
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stolen.Add(1); w.WriteHeader(401) }))
			defer sink.Close()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, sink.URL+"/capture", http.StatusTemporaryRedirect)
			}))
			defer up.Close()
			for _, route := range []string{"/models", "/probe"} {
				w := quotaProviderCall(t, mux, "POST", "/api/quota-providers"+route, map[string]any{"name": "Redirect", "protocol": protocol, "baseURL": up.URL, "apiKey": "must-not-be-forwarded-secret", "models": []string{"m1"}})
				if w.Code == 200 && strings.Contains(w.Body.String(), `"ok":true`) {
					t.Fatalf("redirect accepted: %s", w.Body)
				}
			}
			if stolen.Load() != 0 {
				t.Fatalf("redirect recipient received %d credential-bearing requests", stolen.Load())
			}
		})
	}
}

func TestQuotaProviderErrorsNeverEchoCredentials(t *testing.T) {
	mux := quotaProviderTestMux(t)
	const secret = "upstream-echo-secret-123456"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprintf(w, `{"error":{"message":"bad key %s"}}`, secret)
	}))
	defer up.Close()
	for _, route := range []string{"/models", "/probe"} {
		w := quotaProviderCall(t, mux, "POST", "/api/quota-providers"+route, map[string]any{"name": "Echo", "protocol": "chat", "baseURL": up.URL, "apiKey": secret, "models": []string{"m1"}})
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "bad key") {
			t.Fatalf("upstream error leaked: %s", w.Body)
		}
	}
}

func TestQuotaProviderConcurrentCreatesDoNotLoseEntries(t *testing.T) {
	mux := quotaProviderTestMux(t)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			w := quotaProviderCall(t, mux, "POST", "/api/quota-providers", map[string]any{"name": fmt.Sprint("Provider ", i), "protocol": "chat", "baseURL": "https://example.invalid/v1", "apiKey": "separate-key-secret-123", "models": []string{"m1"}})
			if w.Code != 201 {
				t.Errorf("create: %d %s", w.Code, w.Body)
			}
		})
	}
	wg.Wait()
	stored, err := provider.Stored()
	if err != nil || len(stored) != 4 {
		t.Fatalf("lost provider: count=%d error=%v", len(stored), err)
	}
}

func TestQuotaProviderReadOnlyAdvancedAndLocalKeyless(t *testing.T) {
	mux := quotaProviderTestMux(t)
	id := quotaProviderCreate(t, mux, "http://127.0.0.1:54321/v1", "")
	stored, err := provider.Stored()
	if err != nil || stored[0].Key != "" {
		t.Fatalf("local provider: %+v %v", stored, err)
	}
	stored[0].Headers = map[string]string{"Authorization": "secret-authorization-value"}
	if _, _, err := provider.Restore(stored); err != nil {
		t.Fatal(err)
	}
	list := quotaProviderCall(t, mux, "GET", "/api/quota-providers", nil)
	if strings.Contains(list.Body.String(), "secret-authorization-value") || !strings.Contains(list.Body.String(), `"editable":false`) {
		t.Fatalf("advanced provider exposed: %s", list.Body)
	}
	w := quotaProviderCall(t, mux, "PATCH", "/api/quota-providers/"+id, map[string]any{"name": "Should not lose headers"})
	if w.Code != 409 {
		t.Fatalf("advanced provider editable: %d %s", w.Code, w.Body)
	}
}

func TestQuotaProviderRejectsInvalidOrOversizedForms(t *testing.T) {
	mux := quotaProviderTestMux(t)
	for _, form := range []map[string]any{
		{"name": "Bad", "protocol": "other", "baseURL": "https://example.invalid/v1", "apiKey": "secret"},
		{"name": "Bad", "protocol": "chat", "baseURL": "https://user:password@example.invalid/v1", "apiKey": "secret"},
		{"name": "Bad", "protocol": "chat", "baseURL": "https://example.invalid/v1?key=secret", "apiKey": "secret"},
		{"name": "Bad", "protocol": "chat", "baseURL": "https://example.invalid/v1"},
		{"name": strings.Repeat("x", 70<<10), "protocol": "chat", "baseURL": "https://example.invalid/v1", "apiKey": "secret"},
	} {
		w := quotaProviderCall(t, mux, "POST", "/api/quota-providers", form)
		if w.Code != 400 {
			t.Errorf("invalid form status=%d", w.Code)
		}
	}
	stored, err := provider.Stored()
	if err != nil || len(stored) != 0 {
		t.Fatalf("invalid forms wrote providers: %+v %v", stored, err)
	}
}

func TestQuotaProviderProtocolsListAndProbe(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			mux := quotaProviderTestMux(t)
			var badAuth atomic.Bool
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if protocol == "gemini" {
					if r.Header.Get("x-goog-api-key") != "protocol-secret" {
						badAuth.Store(true)
					}
				} else if r.Header.Get("Authorization") != "Bearer protocol-secret" {
					badAuth.Store(true)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					if protocol == "gemini" {
						io.WriteString(w, `{"models":[{"name":"models/m1","supportedGenerationMethods":["generateContent"]}]}`)
					} else {
						io.WriteString(w, `{"data":[{"id":"m1"}]}`)
					}
					return
				}
				io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"content":[{"type":"text","text":"ok"}],"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
			}))
			defer up.Close()
			for _, route := range []string{"models", "probe"} {
				w := quotaProviderCall(t, mux, "POST", "/api/quota-providers/"+route, map[string]any{"protocol": protocol, "baseURL": up.URL, "apiKey": "protocol-secret", "models": []string{"m1"}})
				if w.Code != 200 || (route == "probe" && !strings.Contains(w.Body.String(), `"ok":true`)) || (route == "models" && !strings.Contains(w.Body.String(), `"m1"`)) {
					t.Fatalf("%s: %d %s", route, w.Code, w.Body)
				}
			}
			if badAuth.Load() {
				t.Fatal("probe/list did not use the selected protocol credentials")
			}
			stored, err := provider.Stored()
			if err != nil || len(stored) != 0 {
				t.Fatalf("draft persisted: %v %v", stored, err)
			}
		})
	}
}

func TestQuotaProviderConcurrentPartialEditsPreserveBothChanges(t *testing.T) {
	mux := quotaProviderTestMux(t)
	id := quotaProviderCreate(t, mux, "https://example.invalid/v1", "preserved-key-secret")
	var wg sync.WaitGroup
	for _, patch := range []map[string]any{{"enabled": false}, {"models": []string{"m2"}}, {"name": "Changed"}} {
		wg.Go(func() {
			w := quotaProviderCall(t, mux, "PATCH", "/api/quota-providers/"+id, patch)
			if w.Code != 200 {
				t.Errorf("patch: %d %s", w.Code, w.Body)
			}
		})
	}
	wg.Wait()
	ps, err := provider.Stored()
	if err != nil || len(ps) != 1 || !ps[0].Off || ps[0].Name != "Changed" || len(ps[0].Models) != 1 || ps[0].Models[0] != "m2" || ps[0].Key != "preserved-key-secret" {
		t.Fatalf("partial edits overwritten: %+v %v", ps, err)
	}
}

func TestQuotaProviderCredentialReuseChecksSchemePortAndUnknownID(t *testing.T) {
	mux := quotaProviderTestMux(t)
	var asked atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1); w.WriteHeader(401) }))
	defer up.Close()
	id := quotaProviderCreate(t, mux, up.URL+"/v1", "existing-key-secret")
	for _, methodPath := range []struct{ method, path string }{{"PATCH", "/" + id}, {"POST", "/models"}, {"POST", "/probe"}} {
		for _, base := range []string{strings.Replace(up.URL, "http://", "https://", 1) + "/v1", "http://127.0.0.1:1/v1"} {
			form := map[string]any{"baseURL": base}
			if methodPath.method == "POST" {
				form["id"] = id
			}
			w := quotaProviderCall(t, mux, methodPath.method, "/api/quota-providers"+methodPath.path, form)
			if w.Code != 400 {
				t.Fatalf("changed origin accepted: %d %s", w.Code, w.Body)
			}
		}
	}
	w := quotaProviderCall(t, mux, "POST", "/api/quota-providers/models", map[string]any{"id": "unknown", "baseURL": up.URL, "protocol": "chat", "apiKey": "new-secret"})
	if w.Code != 404 || asked.Load() != 0 {
		t.Fatalf("unknown provider/network: %d %d", w.Code, asked.Load())
	}
}

func TestQuotaProviderModelEchoAndOversizeAreBounded(t *testing.T) {
	mux := quotaProviderTestMux(t)
	const secret = "reflected-provider-secret"
	var huge atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if huge.Load() {
			fmt.Fprintf(w, `{"data":[{"id":"%s"}]}`, strings.Repeat("x", 2<<20))
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"m1"},{"id":"%s"}]}`, secret)
	}))
	defer up.Close()
	form := map[string]any{"protocol": "chat", "baseURL": up.URL, "apiKey": secret}
	w := quotaProviderCall(t, mux, "POST", "/api/quota-providers/models", form)
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), `"m1"`) {
		t.Fatalf("model echo leaked: %d %s", w.Code, w.Body)
	}
	huge.Store(true)
	w = quotaProviderCall(t, mux, "POST", "/api/quota-providers/models", form)
	if w.Code != 502 || w.Body.Len() > 1024 {
		t.Fatalf("oversized list accepted: %d length=%d", w.Code, w.Body.Len())
	}
}
