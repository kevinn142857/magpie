package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/tokenpack"
)

func TestTokenPackLogsIdentifyPackageMCPWithoutRawPaths(t *testing.T) {
	quotaPanelHome(t)
	p, err := tokenpack.Create("MCP logs", []string{"test/model"}, 1000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := tokenpack.Reserve(p.ID, "test/model", gateway.PackageMCPPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokenpack.Complete(r.ID, 30, 200, true); err != nil {
		t.Fatal(err)
	}
	// Disabled-package metadata is refused before a tool can be identified.
	if err := tokenpack.RecordRefusal(p.ID, "", gateway.PackageMCPPath, 403, tokenpack.ErrDisabled); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	quotaPanelHandler().ServeHTTP(w, httptest.NewRequest("GET", "/api/token-packs/logs", nil))
	var out struct {
		Logs []struct{ Source, Tool, Model string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || len(out.Logs) != 2 {
		t.Fatalf("log response: %d %s", w.Code, w.Body)
	}
	for _, entry := range out.Logs {
		if entry.Source != "mcp" || entry.Model == "" && entry.Tool != "" || entry.Model != "" && entry.Tool != "generate_text" {
			t.Fatalf("MCP source or tool misidentified: %s", w.Body)
		}
	}
}
