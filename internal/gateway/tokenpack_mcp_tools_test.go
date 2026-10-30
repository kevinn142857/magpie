package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/tokenpack"
)

func packageMCPWithCalls(t *testing.T, calls int64) (*Server, tokenpack.Pack, *atomic.Int64) {
	t.Helper()
	s, p, models := packageMCPSetup(t, packageMCPReply)
	key := p.APIKey
	var err error
	p, err = tokenpack.PatchWithMCP(p.ID, nil, nil, &calls)
	if err != nil {
		t.Fatal(err)
	}
	p.APIKey = key
	return s, p, models
}

func packageMCPTool(t *testing.T, h http.Handler, key, tool string, args any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := packageMCPPost(t, h, key, "tools/call", map[string]any{"name": tool, "arguments": args})
	return w, readPackageMCPResult(t, w)
}

func TestTokenPackMCPCallToolsUseIndependentQuotaAndLegacyAlias(t *testing.T) {
	s, p, models := packageMCPWithCalls(t, 3)
	apis := newSearchAPIs(t)
	if err := provider.SetSearchAPI(apis.api("tavily", "tvly-k")); err != nil {
		t.Fatal(err)
	}
	h := s.Front()
	list := packageMCPPost(t, h, p.APIKey, "tools/list", nil)
	readPackageMCPResult(t, list)
	if !strings.Contains(list.Body.String(), `"web_search"`) || !strings.Contains(list.Body.String(), `"web_fetch"`) {
		t.Fatalf("call tools missing: %s", list.Body)
	}
	w, result := packageMCPTool(t, h, p.APIKey, "web_search", map[string]any{"query": "latest Go"})
	if result["isError"] == true || !strings.Contains(w.Body.String(), "Go downloads") {
		t.Fatalf("search failed: %s", w.Body)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["usage"].(map[string]any)["calls"] != float64(1) {
		t.Fatalf("call usage: %s", w.Body)
	}
	p, _ = tokenpack.Get(p.ID)
	if p.MCPUsedCalls != 1 || p.MCPRemainingCalls != 2 || p.UsedTokens != 0 || models.Load() != 0 {
		t.Fatalf("search spent model tokens: %+v models=%d", p, models.Load())
	}
	// The historical search address is an alias of the same scoped package MCP.
	legacyRequest := httptest.NewRequest(http.MethodPost, SearchMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"latest Go"}}}`))
	legacyRequest.Header.Set("Content-Type", "application/json")
	legacyRequest.Header.Set("Authorization", "Bearer "+p.APIKey)
	legacy := httptest.NewRecorder()
	h.ServeHTTP(legacy, legacyRequest)
	if r := readPackageMCPResult(t, legacy); r["isError"] == true {
		t.Fatalf("legacy alias: %s", legacy.Body)
	}
	p, _ = tokenpack.Get(p.ID)
	if p.MCPUsedCalls != 2 || models.Load() != 0 {
		t.Fatalf("alias missed call ledger: %+v", p)
	}
	logs, err := tokenpack.Logs(p.ID, 10)
	if err != nil || len(logs) != 2 {
		t.Fatalf("tool logs: %+v %v", logs, err)
	}
	for _, row := range logs {
		if row.Unit != "calls" || row.Calls != 1 || row.Tool != "web_search" || row.TotalTokens != 0 || row.Outcome != "settled" {
			t.Fatalf("wrong tool accounting: %+v", row)
		}
	}
}

func TestTokenPackMCPMissingAPIAndInvalidToolArgumentsDoNotCharge(t *testing.T) {
	s, p, models := packageMCPWithCalls(t, 4)
	w, result := packageMCPTool(t, s.Front(), p.APIKey, "web_search", map[string]any{"query": "hello"})
	if result["isError"] != true || !strings.Contains(w.Body.String(), "configured") {
		t.Fatalf("unconfigured search: %s", w.Body)
	}
	apis := newSearchAPIs(t)
	if err := provider.SetSearchAPI(apis.api("tavily", "tvly-k")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tool string
		args any
	}{
		{"web_search", map[string]any{"query": ""}},
		{"web_search", map[string]any{"query": "hello", "model": "plan/other"}},
		{"web_search", map[string]any{"query": nil}},
		{"web_fetch", map[string]any{"url": "file:///etc/passwd"}},
		{"web_fetch", map[string]any{"url": "https://example.invalid", "max_chars": -1}},
		{"web_fetch", map[string]any{"url": "https://example.invalid", "offset": -1}},
		{"web_fetch", map[string]any{"url": "https://example.invalid", "offset": nil}},
	} {
		w := packageMCPPost(t, s.Front(), p.APIKey, "tools/call", map[string]any{"name": test.tool, "arguments": test.args})
		if !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Fatalf("bad tool arguments accepted: %s", w.Body)
		}
	}
	p, _ = tokenpack.Get(p.ID)
	if p.MCPUsedCalls != 0 || p.UsedTokens != 0 || models.Load() != 0 {
		t.Fatalf("invalid search charged: %+v", p)
	}
}

func TestTokenPackMCPFailedAcceptedCallIsChargedAndErrorsAreSafe(t *testing.T) {
	s, p, models := packageMCPWithCalls(t, 1)
	const secret = "search-api-private-secret-123"
	var asked atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(401)
		fmt.Fprintf(w, `{"detail":"vendor says key=%s"}`, secret)
	}))
	defer up.Close()
	if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: "tavily", Key: secret, URL: up.URL}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w, result := packageMCPTool(t, s.Front(), p.APIKey, "web_search", map[string]any{"query": "hello"})
		if result["isError"] != true || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "vendor says") {
			t.Fatalf("unsafe search failure: %s", w.Body)
		}
	}
	p, _ = tokenpack.Get(p.ID)
	if p.MCPUsedCalls != 1 || p.MCPRemainingCalls != 0 || p.UsedTokens != 0 || asked.Load() != 1 || models.Load() != 0 {
		t.Fatalf("failed call refunded or retried free: %+v asked=%d", p, asked.Load())
	}
	logs, err := tokenpack.Logs(p.ID, 10)
	if err != nil || len(logs) != 1 || logs[0].Outcome != "failed" {
		t.Fatalf("failed tool log: %+v %v", logs, err)
	}
}

func TestTokenPackMCPFetchReadsTextAndRejectsPrivateTargets(t *testing.T) {
	for _, allowFixture := range []bool{true, false} {
		t.Run(fmt.Sprint(allowFixture), func(t *testing.T) {
			s, p, models := packageMCPWithCalls(t, 2)
			var asked atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "0123456789abcdefghijklmnopqrstuvwxyz")
			}))
			defer up.Close()
			if allowFixture {
				old := webFetchClient
				webFetchClient = up.Client()
				t.Cleanup(func() { webFetchClient = old })
			}
			w, result := packageMCPTool(t, s.Front(), p.APIKey, "web_fetch", map[string]any{"url": up.URL, "max_chars": 10, "offset": 5})
			if allowFixture {
				if result["isError"] == true || !strings.Contains(w.Body.String(), "56789abcde") || asked.Load() != 1 {
					t.Fatalf("fetch: %s calls=%d", w.Body, asked.Load())
				}
			} else if result["isError"] != true || asked.Load() != 0 {
				t.Fatalf("private fetch reached target: %s calls=%d", w.Body, asked.Load())
			}
			p, _ = tokenpack.Get(p.ID)
			if p.MCPUsedCalls != 1 || p.UsedTokens != 0 || models.Load() != 0 {
				t.Fatalf("fetch accounting: %+v", p)
			}
		})
	}
}

func TestTokenPackMCPCallDeadlineAndUpstreamBound(t *testing.T) {
	for _, tool := range []string{"web_search", "web_fetch"} {
		t.Run(tool, func(t *testing.T) {
			s, p, _ := packageMCPWithCalls(t, 2)
			if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: "tavily", Key: "test-key", URL: "https://search.example.invalid"}); err != nil {
				t.Fatal(err)
			}
			body := `{"results":[{"title":"page","url":"https://example.invalid/page","content":"text"}]}` + strings.Repeat(" ", 5<<20)
			read := &packageMCPCountReader{Reader: strings.NewReader(body)}
			var bounded atomic.Bool
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				until, ok := r.Context().Deadline()
				bounded.Store(ok && !until.After(time.Now().Add(31*time.Second)))
				return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(read), Request: r}, nil
			})}
			s.client = client
			old := webFetchClient
			webFetchClient = client
			t.Cleanup(func() { webFetchClient = old })
			args := map[string]any{"query": "hello"}
			if tool == "web_fetch" {
				args = map[string]any{"url": "https://public.example.invalid"}
			}
			w, result := packageMCPTool(t, s.Front(), p.APIKey, tool, args)
			if result["isError"] != true || !bounded.Load() || read.read > (4<<20)+1 || w.Body.Len() > 4<<20 {
				t.Fatalf("unbounded tool: read=%d deadline=%v status=%v", read.read, bounded.Load(), result["isError"])
			}
			p, _ = tokenpack.Get(p.ID)
			if p.MCPUsedCalls != 1 {
				t.Fatalf("oversize call refunded: %+v", p)
			}
		})
	}
}

func TestTokenPackMCPOffSearchAPIsDoNotCharge(t *testing.T) {
	s, p, models := packageMCPWithCalls(t, 3)
	apis := newSearchAPIs(t)
	if err := provider.SetSearchAPI(apis.api("tavily", "tvly-k")); err != nil {
		t.Fatal(err)
	}
	if err := provider.SwitchSearchAPI("tavily", false); err != nil {
		t.Fatal(err)
	}
	w, result := packageMCPTool(t, s.Front(), p.APIKey, "web_search", map[string]any{"query": "hello"})
	p, _ = tokenpack.Get(p.ID)
	if result["isError"] != true || p.MCPUsedCalls != 0 || models.Load() != 0 {
		t.Fatalf("off search charged or succeeded: %s used=%d", w.Body, p.MCPUsedCalls)
	}
}

func TestTokenPackMCPSearchRedactsTheCredentialActuallySent(t *testing.T) {
	s, p, _ := packageMCPWithCalls(t, 3)
	const oldKey, newKey = "old-search-private-secret", "new-search-private-secret"
	var updateFailed atomic.Bool
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: "tavily", Key: newKey, URL: up.URL}); err != nil {
			updateFailed.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"results":[{"title":"page","url":"https://example.invalid/page","content":"echo %s"}]}`, oldKey)
	}))
	defer up.Close()
	if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: "tavily", Key: oldKey, URL: up.URL}); err != nil {
		t.Fatal(err)
	}
	w, result := packageMCPTool(t, s.Front(), p.APIKey, "web_search", map[string]any{"query": "hello"})
	if result["isError"] == true || updateFailed.Load() || strings.Contains(w.Body.String(), oldKey) || strings.Contains(w.Body.String(), newKey) {
		t.Fatalf("rotation leaked a credential: %s", w.Body)
	}
}

func TestTokenPackMCPWaitingForMemoryDoesNotChargeExpiredRequest(t *testing.T) {
	s, p, _ := packageMCPWithCalls(t, 3)
	apis := newSearchAPIs(t)
	if err := provider.SetSearchAPI(apis.api("tavily", "tvly-k")); err != nil {
		t.Fatal(err)
	}
	old := bodyRoom
	bodyRoom = roomOf(32 << 20)
	t.Cleanup(func() { bodyRoom = old })
	hold, release := context.WithCancel(context.Background())
	defer release()
	if err := bodyRoom.take(hold, 32<<20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, packageMCPTestPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"hello"}}}`)).WithContext(ctx)
	r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
	r.Header.Set("Authorization", "Bearer "+p.APIKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Front().ServeHTTP(w, r)
	result := readPackageMCPResult(t, w)
	p, _ = tokenpack.Get(p.ID)
	if result["isError"] != true || p.MCPUsedCalls != 0 {
		t.Fatalf("expired queued work charged: %s calls=%d", w.Body, p.MCPUsedCalls)
	}
}

func TestTokenPackMCPCallHoldsSharedWorkingSetThroughResponse(t *testing.T) {
	s, p, _ := packageMCPWithCalls(t, 3)
	old := bodyRoom
	bodyRoom = roomOf(32 << 20)
	t.Cleanup(func() { bodyRoom = old })
	var held atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held.Store(roomHeld(bodyRoom) >= 32<<20)
		io.WriteString(w, `{"results":[{"title":"page","url":"https://example.invalid","content":"found"}]}`)
	}))
	defer up.Close()
	if err := provider.SetSearchAPI(provider.SearchAPI{Vendor: "tavily", Key: "key", URL: up.URL}); err != nil {
		t.Fatal(err)
	}
	_, result := packageMCPTool(t, s.Front(), p.APIKey, "web_search", map[string]any{"query": "hello"})
	if result["isError"] == true || !held.Load() || roomHeld(bodyRoom) != 0 {
		t.Fatalf("working set missing or leaked: held=%v remaining=%d", held.Load(), roomHeld(bodyRoom))
	}
}
