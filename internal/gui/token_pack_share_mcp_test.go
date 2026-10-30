package gui

import (
	"bytes"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/tokenpack"
)

func TestTokenPackShareProvidesScopedMCPConfiguration(t *testing.T) {
	quotaPanelHome(t)
	t.Setenv("MAGPIE_PUBLIC_URL", "https://relay.example")
	const administrator = "browser-administrator-not-for-sharing"
	const managementKey = "management-administrator-not-for-sharing"
	t.Setenv("MAGPIE_MCP_KEY", managementKey)
	p, err := tokenpack.Create("MCP package", []string{"test/model"}, 1_000_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := tokenpack.Create("Someone else's package", []string{"test/other"}, 500_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	h := webServed("3430", administrator, 0, Handler(webHost{}, nil))
	r := httptest.NewRequest(http.MethodGet, "/share/"+p.ShareToken, nil)
	r.RemoteAddr = "10.0.0.5:50001"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("public share: %d %s", w.Code, w.Body.String())
	}
	before, rest, found := strings.Cut(w.Body.String(), `<code id="mcp-config">`)
	if !found {
		t.Fatal("share page has no copyable MCP configuration")
	}
	config, _, found := strings.Cut(rest, "</code>")
	if !found {
		t.Fatal("MCP configuration has no end")
	}
	var data struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(config)), &data); err != nil {
		t.Fatalf("copyable MCP configuration is not JSON: %v", err)
	}
	server, ok := data.Servers["magpie-package"]
	if !ok || len(data.Servers) != 1 || server.Type != "http" || server.URL != "https://relay.example/mcp/magpie/package" || server.Headers["Authorization"] != "Bearer "+p.APIKey {
		t.Fatalf("wrong package MCP configuration: %+v", data)
	}
	if !strings.Contains(before, "https://relay.example/mcp/magpie/package") {
		t.Fatal("MCP address is not shown separately from the JSON config")
	}
	for _, forbidden := range []string{administrator, managementKey, other.APIKey, "/api/mcp"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("package share exposed a different credential or administrative endpoint: %q", forbidden)
		}
	}
	for _, tool := range []string{"get_quota", "list_models", "generate_text"} {
		if !strings.Contains(w.Body.String(), tool) {
			t.Errorf("share page does not explain tool %s", tool)
		}
	}
	if !strings.Contains(w.Body.String(), `data-copy="mcp-config"`) || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("MCP config is missing copy support or credential protections")
	}
}

func TestTokenPackMCPConfigurationEscapesCopiedJSON(t *testing.T) {
	const endpoint = "https://relay.example/mcp/magpie/package"
	const key = "key-\"\\\n</code><script>untrusted()</script>"
	configuration, err := tokenPackMCPConfig(endpoint, key)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if err := tokenPackShareTemplate.Execute(&body, map[string]any{
		"Pack": tokenpack.Pack{Name: "MCP", APIKey: key}, "BaseURL": "https://relay.example/v1",
		"RootURL": "https://relay.example", "Expiry": "", "Nonce": "fixed-test-nonce",
		"MCPURL": endpoint, "MCPConfig": configuration,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.String(), "<script>untrusted()</script>") {
		t.Fatal("a credential escaped the MCP configuration's text container")
	}
	_, rest, found := strings.Cut(body.String(), `<code id="mcp-config">`)
	if !found {
		t.Fatal("share page has no MCP configuration")
	}
	text, _, found := strings.Cut(rest, "</code>")
	if !found {
		t.Fatal("share page has no complete MCP configuration")
	}
	var decoded struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(text)), &decoded); err != nil {
		t.Fatal("copying the rendered code must produce valid JSON:", err)
	}
	server := decoded.Servers["magpie-package"]
	if server.URL != endpoint || server.Headers["Authorization"] != "Bearer "+key {
		t.Fatal("HTML escaping changed the copied JSON credential")
	}
}

func TestTokenPackShareShowsIndependentCallsAndConditionalTools(t *testing.T) {
	for _, calls := range []int64{0, 20} {
		name := "no-calls"
		if calls > 0 {
			name = "with-calls"
		}
		t.Run(name, func(t *testing.T) {
			var body bytes.Buffer
			if err := tokenPackShareTemplate.Execute(&body, map[string]any{
				"Pack": tokenpack.Pack{Name: "Balances", TotalTokens: 1000, RemainingTokens: 800, MCPTotalCalls: calls, MCPUsedCalls: 2, MCPRemainingCalls: 18},
			}); err != nil {
				t.Fatal(err)
			}
			page := body.String()
			if !strings.Contains(page, "搜索与网页读取") || !strings.Contains(page, "独立计次") {
				t.Fatal("share page does not distinguish the call budget from token usage")
			}
			for _, tool := range []string{"web_search", "web_fetch"} {
				if strings.Contains(page, tool) != (calls > 0) {
					t.Errorf("tool %s visibility does not match call entitlement %d", tool, calls)
				}
			}
		})
	}
}
