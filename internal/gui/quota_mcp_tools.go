package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type quotaMCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
	method      string
	path        string
	properties  map[string]any
	required    []string
}

func quotaMCPTools() []quotaMCPTool {
	str := map[string]any{"type": "string"}
	flag := map[string]any{"type": "boolean"}
	models := map[string]any{"type": "array", "items": str, "minItems": 1}
	tokens := map[string]any{"type": "integer", "minimum": 1, "maximum": 9007199254740991}
	supplier := func(withID bool) map[string]any {
		p := map[string]any{"name": str, "protocol": map[string]any{"type": "string", "enum": []string{"chat", "responses", "anthropic", "gemini"}}, "baseURL": str, "apiKey": str, "models": models, "enabled": flag}
		if withID {
			p["id"] = str
		}
		return p
	}
	makeTool := func(name, description, method, path string, properties map[string]any, required ...string) quotaMCPTool {
		if properties == nil {
			properties = map[string]any{}
		}
		if required == nil {
			required = []string{}
		}
		return quotaMCPTool{Name: name, Description: description, method: method, path: path,
			properties: properties, required: required,
			InputSchema: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
			Annotations: map[string]any{"readOnlyHint": method == http.MethodGet, "destructiveHint": method == http.MethodPatch || name == "settle_token_request", "openWorldHint": name == "get_supplier_models" || name == "test_supplier"}}
	}
	return []quotaMCPTool{
		makeTool("list_suppliers", "List configured API suppliers. Keys are never returned; complex legacy configurations are read-only.", "GET", "/api/quota-providers", nil),
		makeTool("create_supplier", "Save an API supplier. baseURL and protocol select the upstream endpoint; models are upstream model IDs. No vendor request is made until a probe or model call.", "POST", "/api/quota-providers", supplier(false), "name", "protocol", "baseURL", "models"),
		makeTool("update_supplier", "Patch a supplier by id. Omit apiKey to keep it only when the normalized endpoint and protocol remain unchanged. Changing either requires an explicit new key.", "PATCH", "/api/quota-providers/{id}", supplier(true), "id"),
		makeTool("get_supplier_models", "Fetch model IDs from an unsaved supplier draft or existing supplier id. A saved key is reused only for its unchanged endpoint and protocol.", "POST", "/api/quota-providers/models", supplier(true)),
		makeTool("test_supplier", "Send a small paid generation request to the first model of a draft or existing supplier. Reports connection status without returning upstream content.", "POST", "/api/quota-providers/probe", supplier(true)),
		makeTool("list_token_packages", "List lifetime token allowances, remaining and reserved tokens, status and share links. Share links contain access credentials; disclose them only to intended recipients.", "GET", "/api/token-packs", nil),
		makeTool("create_token_package", "Create a model-limited lifetime token package and independent share link. One million tokens is 1000000. Use model IDs from list_models; expiresAt is an optional RFC3339 timestamp.", "POST", "/api/token-packs", map[string]any{"name": str, "models": models, "totalTokens": tokens, "expiresAt": map[string]any{"type": "string", "format": "date-time"}}, "name", "models", "totalTokens"),
		makeTool("update_token_package", "Change a package's TOTAL lifetime allowance, or enable/disable it. totalTokens replaces the total; it is not a top-up delta. It cannot undercut charged or reserved usage.", "PATCH", "/api/token-packs/{id}", map[string]any{"id": str, "totalTokens": tokens, "enabled": flag}, "id"),
		makeTool("list_models", "List model IDs currently available for token packages. Supplier changes are reflected without a restart.", "GET", "/api/token-packs/models", nil),
		makeTool("list_request_logs", "List up to 500 package request records, optionally restricted to packId. Includes completed, refused and unresolved reservations; no prompts or credentials.", "GET", "/api/token-packs/logs", map[string]any{"packId": str, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 500}}),
		makeTool("settle_token_request", "Reconcile an unresolved request by its log id using verified actualTokens, including zero for no usage. This changes the balance and cannot undercut known partial usage; active requests cannot be settled.", "POST", "/api/token-packs/logs/{id}/settle", map[string]any{"id": str, "actualTokens": map[string]any{"type": "integer", "minimum": 0, "maximum": 9007199254740991}}, "id", "actualTokens"),
	}
}

// quotaMCPInvoke routes a fixed tool to the same validated handler used by the
// web console. It makes no HTTP request and cannot choose arbitrary API paths.
func quotaMCPInvoke(ctx context.Context, api http.Handler, name string, raw json.RawMessage) (any, error) {
	var tool *quotaMCPTool
	for _, candidate := range quotaMCPTools() {
		if candidate.Name == name {
			tool = &candidate
			break
		}
	}
	if tool == nil {
		return nil, errors.New("Unknown management tool.")
	}
	args := map[string]json.RawMessage{}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &args); err != nil || args == nil {
			return nil, errors.New("Tool arguments must be an object.")
		}
	}
	for key, value := range args {
		schema, ok := tool.properties[key].(map[string]any)
		if !ok || !quotaMCPArgument(value, schema) {
			return nil, errors.New("Unknown field or invalid argument type.")
		}
	}
	for _, key := range tool.required {
		if _, ok := args[key]; !ok {
			return nil, errors.New("A required tool argument is missing.")
		}
	}
	path := tool.path
	if strings.Contains(path, "{id}") {
		var id string
		_ = json.Unmarshal(args["id"], &id)
		if !quotaMCPIdentifier(id) {
			return nil, errors.New("Invalid record id.")
		}
		path = strings.Replace(path, "{id}", id, 1)
		delete(args, "id")
		if tool.method == http.MethodPatch && len(args) == 0 {
			return nil, errors.New("Provide at least one field to update.")
		}
	}
	if name == "list_request_logs" {
		query := url.Values{}
		if data, ok := args["packId"]; ok {
			var id string
			_ = json.Unmarshal(data, &id)
			if !quotaMCPIdentifier(id) {
				return nil, errors.New("Invalid package id.")
			}
			query.Set("pack", id)
		}
		if data, ok := args["limit"]; ok {
			query.Set("limit", string(data))
		}
		path += "?" + query.Encode()
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, errors.New("Invalid tool arguments.")
	}
	r, err := http.NewRequestWithContext(ctx, tool.method, "http://quota.internal"+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Invalid management request.")
	}
	r.Header.Set("Content-Type", "application/json")
	w := &quotaMCPCapture{header: make(http.Header)}
	api.ServeHTTP(w, r)
	var output map[string]any
	if w.overflow || json.Unmarshal(w.body.Bytes(), &output) != nil || output == nil {
		return quotaMCPToolResult(map[string]any{"error": "Management response unavailable or too large."}, true), nil
	}
	return quotaMCPToolResult(output, w.status >= 400), nil
}

func quotaMCPArgument(raw json.RawMessage, schema map[string]any) bool {
	if string(raw) == "null" {
		return false
	}
	switch schema["type"] {
	case "string":
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		if choices, ok := schema["enum"].([]string); ok {
			for _, choice := range choices {
				if value == choice {
					return true
				}
			}
			return false
		}
		return true
	case "boolean":
		var value bool
		return json.Unmarshal(raw, &value) == nil
	case "integer":
		value, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return false
		}
		min, _ := json.Marshal(schema["minimum"])
		max, _ := json.Marshal(schema["maximum"])
		low, _ := strconv.ParseInt(string(min), 10, 64)
		high, _ := strconv.ParseInt(string(max), 10, 64)
		return value >= low && value <= high
	case "array":
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
			return false
		}
		for _, item := range values {
			var value string
			if string(item) == "null" || json.Unmarshal(item, &value) != nil {
				return false
			}
		}
		return true
	}
	return false
}

func quotaMCPIdentifier(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func quotaMCPToolResult(value map[string]any, failed bool) any {
	data, _ := json.Marshal(value)
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": string(data)}}, "structuredContent": value, "isError": failed}
}

type quotaMCPCapture struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func (w *quotaMCPCapture) Header() http.Header { return w.header }
func (w *quotaMCPCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *quotaMCPCapture) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(b) > (4<<20)-w.body.Len() {
		w.overflow = true
		return 0, io.ErrShortBuffer
	}
	return w.body.Write(b)
}
