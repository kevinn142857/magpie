package gui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/tokenpack"
)

func TestTokenPackManagementCreatesAndEditsSeparateToolCalls(t *testing.T) {
	quotaPanelHome(t)
	t.Setenv("MAGPIE_PUBLIC_URL", "https://relay.example")
	h := quotaPanelHandler()
	w := quotaProviderCall(t, h, "POST", "/api/token-packs", map[string]any{"name": "Both quotas", "models": []string{"test/model"}, "totalTokens": 10000, "mcpTotalCalls": 3})
	if w.Code != 201 {
		t.Fatalf("create separate calls: %d %s", w.Code, w.Body)
	}
	var out struct{ Pack tokenpack.Pack }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Pack.MCPRemainingCalls != 3 {
		t.Fatalf("quota missing: %s %v", w.Body, err)
	}
	id, err := tokenpack.ReserveMCPCall(out.Pack.ID, "web_fetch", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenpack.CompleteMCPCall(id, 502); err != nil {
		t.Fatal(err)
	}
	w = quotaProviderCall(t, h, "PATCH", "/api/token-packs/"+out.Pack.ID, map[string]any{"mcpTotalCalls": 5})
	if w.Code != 200 {
		t.Fatalf("edit separate calls: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Pack.MCPUsedCalls != 1 || out.Pack.MCPRemainingCalls != 4 || out.Pack.UsedTokens != 0 {
		t.Fatalf("mixed quotas: %s %v", w.Body, err)
	}
	w = quotaProviderCall(t, h, "GET", "/api/token-packs/logs", nil)
	if !strings.Contains(w.Body.String(), `"unit":"calls"`) || !strings.Contains(w.Body.String(), `"calls":1`) || !strings.Contains(w.Body.String(), `"tool":"web_fetch"`) {
		t.Fatalf("call log unit lost: %s", w.Body)
	}
}
