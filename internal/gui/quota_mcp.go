package gui

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var quotaMCPVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

type quotaMCPMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// quotaMCP provides stateless Streamable HTTP for the initialize-era protocol.
// It never trusts a browser cookie or a gateway package key. Unsupported modern
// revisions get an explicit version error, allowing dual-era SDKs to fall back.
func quotaMCP(administrator string, next http.Handler) http.Handler {
	api := quotaPanelHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !quotaPanelEnabled() || r.URL.Path != "/api/mcp" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		key := os.Getenv("MAGPIE_MCP_KEY")
		if !quotaMCPKeyValid(key) || subtle.ConstantTimeCompare([]byte(key), []byte(administrator)) == 1 {
			packJSONError(w, http.StatusServiceUnavailable, "Configure a separate MAGPIE_MCP_KEY to enable management MCP.")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				packJSONError(w, http.StatusForbidden, "Cross-origin MCP requests are refused.")
				return
			}
		}
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(key)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="magpie-management"`)
			packJSONError(w, http.StatusUnauthorized, "A management MCP bearer credential is required.")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			packJSONError(w, http.StatusMethodNotAllowed, "Use POST for stateless management MCP.")
			return
		}
		if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
			packJSONError(w, http.StatusUnsupportedMediaType, "Use Content-Type: application/json.")
			return
		}
		if accept := r.Header.Get("Accept"); accept != "" && !strings.Contains(accept, "application/json") && !strings.Contains(accept, "*/*") {
			packJSONError(w, http.StatusNotAcceptable, "Accept application/json for management MCP.")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			packJSONError(w, http.StatusRequestEntityTooLarge, "MCP request exceeds the 64 KiB limit.")
			return
		}
		var message quotaMCPMessage
		if !json.Valid(body) {
			quotaMCPError(w, 400, nil, -32700, "Invalid JSON request.", nil)
			return
		}
		if json.Unmarshal(body, &message) != nil || message.JSONRPC != "2.0" || message.Method == "" || (message.ID != nil && !quotaMCPIDValid(message.ID)) {
			quotaMCPError(w, 400, nil, -32600, "Invalid JSON-RPC request.", nil)
			return
		}
		if len(message.Params) != 0 && message.Params[0] != '{' {
			quotaMCPError(w, 200, message.ID, -32602, "MCP params must be an object.", nil)
			return
		}
		version := r.Header.Get("MCP-Protocol-Version")
		if version != "" && !quotaMCPVersionSupported(version) {
			quotaMCPError(w, 400, message.ID, -32022, "Unsupported protocol version.", map[string]any{"supported": quotaMCPVersions, "requested": version})
			return
		}
		// Notifications are acknowledged without executing any management tool.
		if message.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch message.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(message.Params, &params) != nil || params.ProtocolVersion == "" {
				quotaMCPError(w, 200, message.ID, -32602, "Provide a protocolVersion.", nil)
				return
			}
			selected := params.ProtocolVersion
			if !quotaMCPVersionSupported(selected) {
				selected = quotaMCPVersions[0]
			}
			result = map[string]any{"protocolVersion": selected, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo":   map[string]string{"name": "magpie-quota-admin", "version": "1.0.0"},
				"instructions": "Administrative tools manage suppliers and lifetime token packages. Share links grant package access. Keep supplier and management credentials private. Test supplier calls can incur vendor charges."}
		case "server/discover":
			var params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			}
			_ = json.Unmarshal(message.Params, &params)
			requested := ""
			_ = json.Unmarshal(params.Meta["io.modelcontextprotocol/protocolVersion"], &requested)
			quotaMCPError(w, 400, message.ID, -32022, "This server supports initialize-era MCP. Retry with a supported version.", map[string]any{"supported": quotaMCPVersions, "requested": requested})
			return
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": quotaMCPTools()}
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(message.Params, &params) != nil || params.Name == "" {
				quotaMCPError(w, 200, message.ID, -32602, "Provide a tool name and arguments object.", nil)
				return
			}
			result, err = quotaMCPInvoke(r.Context(), api, params.Name, params.Arguments)
			if err != nil {
				quotaMCPError(w, 200, message.ID, -32602, err.Error(), nil)
				return
			}
		default:
			quotaMCPError(w, 200, message.ID, -32601, "Method not found.", nil)
			return
		}
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
	})
}

func quotaMCPKeyValid(key string) bool {
	return len(key) >= 32 && len(key) <= 256 && strings.Trim(key, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~") == ""
}

func quotaMCPVersionSupported(version string) bool {
	for _, supported := range quotaMCPVersions {
		if version == supported {
			return true
		}
	}
	return false
}

func quotaMCPIDValid(id json.RawMessage) bool {
	var text string
	if len(id) > 0 && id[0] == '"' {
		return json.Unmarshal(id, &text) == nil
	}
	if bytes.Equal(id, []byte("null")) {
		return false
	}
	var integer int64
	return json.Unmarshal(id, &integer) == nil
}

func quotaMCPError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	err := map[string]any{"code": code, "message": message}
	if data != nil {
		err["data"] = data
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "error": err})
}
