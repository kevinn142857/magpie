package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/tokenpack"
)

const quotaMCPTestKey = "mcp-independent-test-credential-1234567890"

func quotaMCPCall(t *testing.T, h http.Handler, body, credential string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://relay.example/api/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func quotaMCPTestHandler(t *testing.T) http.Handler {
	t.Helper()
	quotaPanelHome(t)
	t.Setenv("MAGPIE_MCP_KEY", quotaMCPTestKey)
	t.Setenv("MAGPIE_PUBLIC_URL", "https://relay.example")
	return webServed("3430", "browser-administrator-1234567890123456", 0, Handler(webHost{}, nil))
}

func quotaMCPToolCall(t *testing.T, h http.Handler, name string, args any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	return quotaMCPCall(t, h, string(body), quotaMCPTestKey)
}

func TestQuotaMCPToolsShareProviderAndPackageValidation(t *testing.T) {
	h := quotaMCPTestHandler(t)
	const secret = "upstream-secret-must-not-be-returned"
	w := quotaMCPToolCall(t, h, "create_supplier", map[string]any{"name": "Supplier", "protocol": "chat", "baseURL": "https://api.example.invalid", "apiKey": secret, "models": []string{"m1"}})
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("create supplier: %d %s", w.Code, w.Body)
	}
	var created struct {
		Result struct {
			StructuredContent struct {
				Provider struct{ ID string }
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Result.StructuredContent.Provider.ID
	if id == "" {
		t.Fatalf("missing supplier id: %s", w.Body)
	}
	for _, name := range []string{"list_suppliers", "list_models", "list_token_packages", "list_request_logs"} {
		w = quotaMCPToolCall(t, h, name, map[string]any{})
		if w.Code != 200 || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	w = quotaMCPToolCall(t, h, "update_supplier", map[string]any{"id": id, "baseURL": "https://different.example.invalid"})
	if !strings.Contains(w.Body.String(), `"isError":true`) || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("changed endpoint reused credential: %s", w.Body)
	}
	w = quotaMCPToolCall(t, h, "create_token_package", map[string]any{"name": "One million", "models": []string{id + "/m1"}, "totalTokens": 1000000})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"shareURL"`) || strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("create token package: %d %s", w.Code, w.Body)
	}
	packs, err := tokenpack.List()
	if err != nil || len(packs) != 1 || packs[0].TotalTokens != 1000000 {
		t.Fatalf("MCP did not use package storage: %d %v", len(packs), err)
	}
	w = quotaMCPToolCall(t, h, "update_token_package", map[string]any{"id": packs[0].ID, "enabled": false})
	if strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("disable package: %s", w.Body)
	}
	p, err := tokenpack.Get(packs[0].ID)
	if err != nil || p.Enabled {
		t.Fatalf("MCP disable not persisted: %v", err)
	}
}

func TestQuotaMCPRejectsInvalidArgumentsBeforeMutation(t *testing.T) {
	h := quotaMCPTestHandler(t)
	for _, args := range []any{
		map[string]any{"name": "bad", "models": []string{"test/m1"}, "totalTokens": 1, "unexpected": "secret"},
		map[string]any{"name": "bad", "models": []string{"test/m1"}, "totalTokens": 1.5},
		map[string]any{"name": "bad", "models": []any{"test/m1", nil}, "totalTokens": 1},
		map[string]any{"name": "bad", "models": []string{"test/m1"}, "totalTokens": nil},
		map[string]any{"name": "bad", "models": []string{"test/m1"}, "totalTokens": 9007199254740992},
		map[string]any{"name": "bad", "models": []string{"test/m1"}},
		[]string{"not-an-object"}, nil,
	} {
		w := quotaMCPToolCall(t, h, "create_token_package", args)
		if !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Errorf("invalid args accepted: %s", w.Body)
		}
	}
	for _, args := range []any{map[string]any{"id": "../models", "enabled": false}, map[string]any{"id": "fake"}} {
		w := quotaMCPToolCall(t, h, "update_token_package", args)
		if !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Errorf("invalid patch accepted: %s", w.Body)
		}
	}
	if packs, err := tokenpack.List(); err != nil || len(packs) != 0 {
		t.Fatalf("invalid call created package: %d %v", len(packs), err)
	}
}

func TestQuotaMCPTransportBoundaries(t *testing.T) {
	h := quotaMCPTestHandler(t)
	for _, tc := range []struct {
		name, method, body, origin, version string
		status                              int
	}{
		{"get", "GET", "", "", "", 405},
		{"delete", "DELETE", "", "", "", 405},
		{"origin", "POST", `{}`, "https://evil.example", "", 403},
		{"null origin", "POST", `{}`, "null", "", 403},
		{"malformed", "POST", `{`, "", "", 400},
		{"batch", "POST", `[]`, "", "", 400},
		{"null id", "POST", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, "", "", 400},
		{"fraction id", "POST", `{"jsonrpc":"2.0","id":1.1,"method":"ping"}`, "", "", 400},
		{"oversized", "POST", strings.Repeat(" ", 65<<10), "", "", 413},
		{"version", "POST", `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, "", "2026-07-28", 400},
		{"notification", "POST", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_token_package","arguments":{"name":"No","models":["m"],"totalTokens":1}}}`, "", "", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://relay.example/api/mcp", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+quotaMCPTestKey)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("MCP-Protocol-Version", tc.version)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body)
			}
			if tc.name == "version" && (!strings.Contains(w.Body.String(), `"code":-32022`) || !strings.Contains(w.Body.String(), `"supported"`)) {
				t.Fatalf("missing downgrade information: %s", w.Body)
			}
		})
	}
	if packs, err := tokenpack.List(); err != nil || len(packs) != 0 {
		t.Fatalf("notification mutated storage: %d %v", len(packs), err)
	}
}

func TestQuotaMCPDisabledWithoutSeparateKey(t *testing.T) {
	h := quotaMCPTestHandler(t)
	for _, key := range []string{"", "short", "spaces are forbidden even with enough characters", "browser-administrator-1234567890123456"} {
		t.Setenv("MAGPIE_MCP_KEY", key)
		w := quotaMCPCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, key)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("invalid management key enabled MCP: %d", w.Code)
		}
	}
}

func TestQuotaMCPSettlementUsesLedgerRules(t *testing.T) {
	h := quotaMCPTestHandler(t)
	p, err := tokenpack.Create("Held", []string{"test/m1"}, 100, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := tokenpack.Reserve(p.ID, "test/m1", "/v1/chat/completions", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tokenpack.Complete(reservation.ID, 30, 502, false); err != nil {
		t.Fatal(err)
	}
	for _, actual := range []int{29, 30} {
		w := quotaMCPToolCall(t, h, "settle_token_request", map[string]any{"id": reservation.ID, "actualTokens": actual})
		want := fmt.Sprintf(`"isError":%t`, actual < 30)
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("settlement %d: %s", actual, w.Body)
		}
	}
	p, err = tokenpack.Get(p.ID)
	if err != nil || p.UsedTokens != 30 || p.ReservedTokens != 0 {
		t.Fatalf("incorrect settled balance: %+v %v", p, err)
	}
}

func TestQuotaMCPListsElevenValidTools(t *testing.T) {
	h := quotaMCPTestHandler(t)
	w := quotaMCPCall(t, h, `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`, quotaMCPTestKey)
	var out struct {
		Result struct{ Tools []quotaMCPTool }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Result.Tools) != 11 {
		t.Fatalf("tool definitions: %s", w.Body)
	}
	seen := map[string]bool{}
	for _, tool := range out.Result.Tools {
		if seen[tool.Name] || tool.InputSchema["type"] != "object" || tool.InputSchema["additionalProperties"] != false || tool.Description == "" {
			t.Fatalf("invalid tool %s", tool.Name)
		}
		seen[tool.Name] = true
	}
}

func TestQuotaMCPProtocolErrorsHaveStandardCodes(t *testing.T) {
	h := quotaMCPTestHandler(t)
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{`, -32700},
		{`[]`, -32600},
		{`"string"`, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":[]}`, -32602},
	} {
		w := quotaMCPCall(t, h, tc.body, quotaMCPTestKey)
		if !strings.Contains(w.Body.String(), fmt.Sprintf(`"code":%d`, tc.code)) {
			t.Fatalf("wrong protocol error: %s", w.Body)
		}
	}
	w := quotaMCPCall(t, h, `{"jsonrpc":"2.0","id":"probe","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, quotaMCPTestKey)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"requested":"2026-07-28"`) || !strings.Contains(w.Body.String(), `"id":"probe"`) {
		t.Fatalf("invalid fallback response: %s", w.Body)
	}
}

func TestQuotaMCPRequiresIndependentCredential(t *testing.T) {
	quotaPanelHome(t)
	t.Setenv("MAGPIE_MCP_KEY", quotaMCPTestKey)
	h := webServed("3430", "browser-administrator-1234567890123456", 0, Handler(webHost{}, nil))
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`
	for _, credential := range []string{"", "browser-administrator-1234567890123456", "sk-magpie-key-not-an-admin"} {
		if w := quotaMCPCall(t, h, init, credential); w.Code != http.StatusUnauthorized {
			t.Errorf("unauthorized MCP: %d %s", w.Code, w.Body)
		}
	}
	w := quotaMCPCall(t, h, init, quotaMCPTestKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("MCP initialize: %d %s", w.Code, w.Body)
	}
	var envelope struct{ Result map[string]any }
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Result["serverInfo"] == nil {
		t.Fatalf("invalid initialize result: %s", w.Body)
	}
}
