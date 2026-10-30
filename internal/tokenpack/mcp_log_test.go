package tokenpack

import (
	"testing"
)

func TestRefusalRetainsOnlyFixedPackageMCPEndpoint(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	for _, endpoint := range []string{"/mcp/magpie/package", "/mcp/magpie/hub/private-secret"} {
		if err := RecordRefusal(p.ID, "plan/m1", endpoint, 403, ErrUnsupported); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := Logs(p.ID, 10)
	if err != nil || len(logs) != 2 {
		t.Fatalf("refusal logs: %v %v", logs, err)
	}
	seen := map[string]bool{}
	for _, entry := range logs {
		seen[entry.Endpoint] = true
	}
	if !seen["/mcp/magpie/package"] || !seen["unsupported"] {
		t.Fatalf("MCP endpoint classification: %v", seen)
	}
}
