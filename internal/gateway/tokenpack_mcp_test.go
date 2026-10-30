package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/tokenpack"
)

const packageMCPTestPath = "/mcp/magpie/package"

func packageMCPPost(t *testing.T, h http.Handler, key, method string, params any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, packageMCPTestPath, strings.NewReader(string(body)))
	r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func readPackageMCPResult(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("package MCP: %d %s", w.Code, w.Body)
	}
	var reply struct {
		Result map[string]any
		Error  any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.Error != nil {
		t.Fatalf("RPC result: %s error=%v", w.Body, err)
	}
	return reply.Result
}

func packageMCPSetup(t *testing.T, response string) (*Server, tokenpack.Pack, *atomic.Int64) {
	t.Helper()
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	var calls atomic.Int64
	serveOn(t, "plan", "upstream-private-secret", []string{"m1", "other"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["stream"] == true || body["model"] != "m1" {
			t.Errorf("unexpected request: %v", body)
		}
		limit, _ := body["max_completion_tokens"].(float64)
		if limit == 0 {
			limit, _ = body["max_tokens"].(float64)
		}
		if limit <= 0 || limit > 10000 {
			t.Errorf("unbounded generation limit=%v", limit)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, response)
	}))
	p, err := tokenpack.Create("MCP package", []string{"plan/m1"}, 10000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return New(), p, &calls
}

const packageMCPReply = `{"id":"done","choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":300,"completion_tokens":50}}`

func TestTokenPackMCPMetadataUsesOnlyPackageAndNoTokens(t *testing.T) {
	s, p, calls := packageMCPSetup(t, packageMCPReply)
	h := s.Front()
	init := readPackageMCPResult(t, packageMCPPost(t, h, p.APIKey, "initialize", map[string]any{"protocolVersion": "2025-03-26"}))
	if init["protocolVersion"] != "2025-03-26" {
		t.Fatalf("version: %v", init)
	}
	tools := readPackageMCPResult(t, packageMCPPost(t, h, p.APIKey, "tools/list", nil))
	data, _ := json.Marshal(tools)
	for _, name := range []string{"get_quota", "list_models", "generate_text"} {
		if !strings.Contains(string(data), name) {
			t.Fatalf("missing %s: %s", name, data)
		}
	}
	for _, name := range []string{"get_quota", "list_models"} {
		w := packageMCPPost(t, h, p.APIKey, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
		result := readPackageMCPResult(t, w)
		if result["isError"] == true || result["structuredContent"] == nil {
			t.Fatalf("metadata: %s", w.Body)
		}
		if strings.Contains(w.Body.String(), p.APIKey) || strings.Contains(w.Body.String(), p.ShareToken) || strings.Contains(w.Body.String(), "upstream-private-secret") || strings.Contains(w.Body.String(), "plan/other") {
			t.Fatalf("metadata leaked: %s", w.Body)
		}
	}
	p, _ = tokenpack.Get(p.ID)
	logs, err := tokenpack.Logs(p.ID, 10)
	if err != nil || p.UsedTokens != 0 || p.ReservedTokens != 0 || len(logs) != 0 || calls.Load() != 0 {
		t.Fatalf("metadata incurred usage: %+v %v %d", p, err, calls.Load())
	}
}

func TestTokenPackMCPGenerateSettlesAndEnforcesArguments(t *testing.T) {
	s, p, calls := packageMCPSetup(t, packageMCPReply)
	h := s.Front()
	w := packageMCPPost(t, h, p.APIKey, "tools/call", map[string]any{"name": "generate_text", "arguments": map[string]any{"model": "plan/m1", "prompt": "Hello", "system": "Be brief", "maxTokens": 1000000}})
	result := readPackageMCPResult(t, w)
	if result["isError"] == true || !strings.Contains(w.Body.String(), "answer") {
		t.Fatalf("generation: %s", w.Body)
	}
	structured := result["structuredContent"].(map[string]any)
	usage := structured["usage"].(map[string]any)
	if structured["model"] != "plan/m1" || usage["totalTokens"] != float64(350) || usage["complete"] != true {
		t.Fatalf("usage: %s", w.Body)
	}
	p, _ = tokenpack.Get(p.ID)
	if p.UsedTokens != 350 || p.ReservedTokens != 0 || calls.Load() != 1 {
		t.Fatalf("settlement: %+v calls=%d", p, calls.Load())
	}
	logs, err := tokenpack.Logs(p.ID, 1)
	if err != nil || len(logs) != 1 || logs[0].Endpoint != packageMCPTestPath {
		t.Fatalf("MCP source not recorded: %+v %v", logs, err)
	}
	for _, args := range []map[string]any{
		{"model": "plan/other", "prompt": "outside"},
		{"model": "plan/m1", "prompt": "hi", "tools": []any{}},
		{"model": "plan/m1", "prompt": "hi", "maxTokens": -1},
		{"model": "plan/m1", "prompt": "hi", "maxTokens": 1.5},
		{"model": "plan/m1", "prompt": "hi", "maxTokens": nil},
		{"model": "plan/m1", "prompt": "hi", "system": nil},
		{"model": "plan/m1", "prompt": ""},
		{"prompt": "missing model"},
	} {
		w := packageMCPPost(t, h, p.APIKey, "tools/call", map[string]any{"name": "generate_text", "arguments": args})
		if w.Code == 200 && !strings.Contains(w.Body.String(), `"isError":true`) && !strings.Contains(w.Body.String(), `"error":`) {
			t.Errorf("invalid arguments succeeded: %s", w.Body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid request reached supplier: %d", calls.Load())
	}
	p, _ = tokenpack.Get(p.ID)
	if p.UsedTokens != 350 || p.ReservedTokens != 0 {
		t.Fatalf("invalid args changed allowance: %+v", p)
	}
}

func TestTokenPackMCPUnknownUsageKeepsReservation(t *testing.T) {
	s, p, _ := packageMCPSetup(t, `{"choices":[{"message":{"content":"answer"}}],"usage":{"prompt_tokens":300}}`)
	w := packageMCPPost(t, s.Front(), p.APIKey, "tools/call", map[string]any{"name": "generate_text", "arguments": map[string]any{"model": "plan/m1", "prompt": "Hello"}})
	result := readPackageMCPResult(t, w)
	structured := result["structuredContent"].(map[string]any)
	if structured["usage"].(map[string]any)["complete"] != false {
		t.Fatalf("unknown usage claimed complete: %s", w.Body)
	}
	p, _ = tokenpack.Get(p.ID)
	if p.UsedTokens != 0 || p.ReservedTokens != 10000 {
		t.Fatalf("unknown usage released: %+v", p)
	}
}

func TestTokenPackMCPAuthenticationAndPackageState(t *testing.T) {
	s, p, calls := packageMCPSetup(t, packageMCPReply)
	h := s.Front()
	ordinary, err := access.Update("add-key", access.Change{Name: "ordinary"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", ordinary} {
		if w := packageMCPPost(t, h, key, "tools/list", nil); w.Code != 401 {
			t.Fatalf("ordinary caller: %d %s", w.Code, w.Body)
		}
	}
	if _, err := tokenpack.Patch(p.ID, nil, new(false)); err != nil {
		t.Fatal(err)
	}
	if w := packageMCPPost(t, h, p.APIKey, "tools/list", nil); w.Code != 403 {
		t.Fatalf("disabled: %d %s", w.Code, w.Body)
	}
	if _, err := tokenpack.Patch(p.ID, nil, new(true)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", tokenpack.Path())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE token_packs SET expires=1 WHERE id=?", p.ID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if w := packageMCPPost(t, h, p.APIKey, "tools/list", nil); w.Code != 410 {
		t.Fatalf("expired: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("guard reached provider: %d", calls.Load())
	}
}

func TestTokenPackMCPDoesNotOpenOtherMCPServers(t *testing.T) {
	s, p, _ := packageMCPSetup(t, packageMCPReply)
	for _, path := range []string{SearchMCPPath, VisionMCPPath, ImageMCPPath, ToolsMCPPath, HubMCPAll, "/mcp/run/private", "/mcp/private", packageMCPTestPath + "/"} {
		if w := packRequest(s.Front(), p.APIKey, path, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); w.Code != 403 {
			t.Errorf("other MCP opened at %s: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestTokenPackMCPResponseIsBoundedAndErrorsAreSanitized(t *testing.T) {
	for _, huge := range []bool{false, true} {
		t.Run(fmt.Sprint(huge), func(t *testing.T) {
			reply := `{"error":{"message":"upstream-private-secret"}}`
			if huge {
				reply = fmt.Sprintf(`{"choices":[{"message":{"content":"%s"}}],"usage":{"prompt_tokens":300,"completion_tokens":50}}`, strings.Repeat("a", 5<<20))
			}
			s, p, _ := packageMCPSetup(t, reply)
			w := packageMCPPost(t, s.Front(), p.APIKey, "tools/call", map[string]any{"name": "generate_text", "arguments": map[string]any{"model": "plan/m1", "prompt": "hi"}})
			result := readPackageMCPResult(t, w)
			if result["isError"] != true || strings.Contains(w.Body.String(), "upstream-private-secret") || w.Body.Len() > 4096 {
				t.Fatalf("unsafe failure length=%d: %.1000s", w.Body.Len(), w.Body.String())
			}
			if huge {
				p, _ = tokenpack.Get(p.ID)
				if p.ReservedTokens != 10000 {
					t.Fatalf("oversized response released allowance: %+v", p)
				}
			}
		})
	}
}

func TestTokenPackRejectsServerSidePaidToolsBeforeSupplier(t *testing.T) {
	for _, fragment := range []string{
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]`,
		`"tools":[{"type":"web_search_preview"}]`,
		`"tools":[{"type":"code_interpreter","container":{"type":"auto"}}]`,
		`"tools":[{"type":"file_search","vector_store_ids":["vs_example"]}]`,
		`"tools":[{"type":"mcp","server_url":"https://example.invalid"}]`,
		`"tools":[{"googleSearch":{}}]`,
		`"tools":[{"google_search":{}}]`,
		`"tools":[{"codeExecution":{}}]`,
		`"tools":[{"urlContext":{}}]`,
		`"web_search_options":{}`,
		`"plugins":[{"id":"web"}]`,
	} {
		t.Run(fragment, func(t *testing.T) {
			s, p, calls := packageMCPSetup(t, packageMCPReply)
			body := `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}],` + fragment + `}`
			w := packRequest(s.Front(), p.APIKey, "/v1/chat/completions", body)
			if w.Code != 400 || calls.Load() != 0 {
				t.Fatalf("paid service reached supplier: status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestTokenPackKeepsClientFunctionAndCustomTools(t *testing.T) {
	for _, fragment := range []string{
		`"tools":[{"type":"function","function":{"name":"web_search","parameters":{"type":"object","properties":{"googleSearch":{"type":"string"}}}}}]`,
		`"tools":[{"type":"custom","name":"text_tool"}]`,
		`"tools":[{"name":"read","input_schema":{"type":"object"}}]`,
		`"tools":[{"functionDeclarations":[{"name":"read","parameters":{"type":"object"}}]}]`,
	} {
		t.Run(fragment, func(t *testing.T) {
			s, p, calls := packageMCPSetup(t, packageMCPReply)
			body := `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}],` + fragment + `}`
			w := packRequest(s.Front(), p.APIKey, "/v1/chat/completions", body)
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatalf("client tool rejected: %d %s calls=%d", w.Code, w.Body, calls.Load())
			}
		})
	}
}

type packageMCPCountReader struct {
	io.Reader
	read int64
}

func (r *packageMCPCountReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.read += int64(n)
	return n, err
}

func TestTokenPackMCPBoundsUpstreamBeforeWholeJSONBuffer(t *testing.T) {
	s, p, _ := packageMCPSetup(t, packageMCPReply)
	body := fmt.Sprintf(`{"choices":[{"message":{"content":"%s"}}],"usage":{"prompt_tokens":300,"completion_tokens":50}}`, strings.Repeat("x", 6<<20))
	read := &packageMCPCountReader{Reader: strings.NewReader(body)}
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(read), Request: r}, nil
	})}
	w := packageMCPPost(t, s.Front(), p.APIKey, "tools/call", map[string]any{"name": "generate_text", "arguments": map[string]any{"model": "plan/m1", "prompt": "hi"}})
	result := readPackageMCPResult(t, w)
	if result["isError"] != true || read.read > (4<<20)+1 {
		t.Fatalf("upstream read was unbounded: read=%d result=%v", read.read, result["isError"])
	}
	p, _ = tokenpack.Get(p.ID)
	if p.ReservedTokens != 10000 {
		t.Fatalf("partial upstream released reservation: %+v", p)
	}
}

func TestTokenPackMCPExhaustionStillAllowsQuotaMetadata(t *testing.T) {
	reply := strings.Replace(packageMCPReply, `"prompt_tokens":300`, `"prompt_tokens":9950`, 1)
	s, p, calls := packageMCPSetup(t, reply)
	h := s.Front()
	params := map[string]any{"name": "generate_text", "arguments": map[string]any{"model": "plan/m1", "prompt": "hi"}}
	first := readPackageMCPResult(t, packageMCPPost(t, h, p.APIKey, "tools/call", params))
	if first["isError"] == true {
		t.Fatalf("first call: %v", first)
	}
	second := readPackageMCPResult(t, packageMCPPost(t, h, p.APIKey, "tools/call", params))
	if second["isError"] != true || second["structuredContent"].(map[string]any)["statusCode"] != float64(429) || calls.Load() != 1 {
		t.Fatalf("exhaustion: %v calls=%d", second, calls.Load())
	}
	quota := readPackageMCPResult(t, packageMCPPost(t, h, p.APIKey, "tools/call", map[string]any{"name": "get_quota"}))
	q := quota["structuredContent"].(map[string]any)["quota"].(map[string]any)
	if q["usedTokens"] != float64(10000) || q["remainingTokens"] != float64(0) {
		t.Fatalf("quota: %v", q)
	}
}

func TestTokenPackMCPRejectsOversizedMessagesBeforeGeneration(t *testing.T) {
	s, p, calls := packageMCPSetup(t, packageMCPReply)
	w := packageMCPPost(t, s.Front(), p.APIKey, "tools/call", map[string]any{"name": "generate_text", "arguments": map[string]any{"model": "plan/m1", "prompt": strings.Repeat("x", 65<<10)}})
	if w.Code != 413 || calls.Load() != 0 {
		t.Fatalf("oversized MCP: %d calls=%d", w.Code, calls.Load())
	}
	p, _ = tokenpack.Get(p.ID)
	if p.UsedTokens != 0 || p.ReservedTokens != 0 {
		t.Fatalf("oversize changed allowance: %+v", p)
	}
}

func TestTokenPackMCPSerializedResultHasBound(t *testing.T) {
	reply := fmt.Sprintf(`{"choices":[{"message":{"content":"%s"}}],"usage":{"prompt_tokens":300,"completion_tokens":50}}`, strings.Repeat("a", 3<<20))
	s, p, _ := packageMCPSetup(t, reply)
	w := packageMCPPost(t, s.Front(), p.APIKey, "tools/call", map[string]any{"name": "generate_text", "arguments": map[string]any{"model": "plan/m1", "prompt": "hi"}})
	result := readPackageMCPResult(t, w)
	if result["isError"] != true || w.Body.Len() > 4<<20 {
		t.Fatalf("serialized MCP result not bounded: length=%d", w.Body.Len())
	}
}

func packageMCPRaw(t *testing.T, h http.Handler, key, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, packageMCPTestPath, strings.NewReader(body))
	r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestTokenPackMCPRejectsBatchesBeforeAnyGeneration(t *testing.T) {
	for _, method := range []string{"generate_text", "get_quota"} {
		t.Run(method, func(t *testing.T) {
			s, p, calls := packageMCPSetup(t, packageMCPReply)
			call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, method, map[string]string{"generate_text": `{"model":"plan/m1","prompt":"hi"}`, "get_quota": `{}`}[method])
			w := packageMCPRaw(t, s.Front(), p.APIKey, "["+call+","+call+"]")
			if w.Code != 400 || calls.Load() != 0 {
				t.Fatalf("batch executed: status=%d supplier_calls=%d", w.Code, calls.Load())
			}
			p, _ = tokenpack.Get(p.ID)
			if p.UsedTokens != 0 || p.ReservedTokens != 0 {
				t.Fatalf("batch changed allowance: %+v", p)
			}
		})
	}
}

func TestTokenPackMCPValidatesEnvelopeAndProtocolHeader(t *testing.T) {
	s, p, calls := packageMCPSetup(t, packageMCPReply)
	for _, body := range []string{
		`{"jsonrpc":"1.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":true,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":{},"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":[],"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":42}`,
		`{"jsonrpc":"2.0","id":1,"method":""}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
	} {
		if w := packageMCPRaw(t, s.Front(), p.APIKey, body); w.Code != 400 {
			t.Errorf("invalid envelope accepted: status=%d body=%s", w.Code, body)
		}
	}
	for _, version := range []string{"2024-11-05", "2026-01-01"} {
		w := packageMCPRaw(t, s.Front(), p.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "MCP-Protocol-Version", version)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"supported"`) || !strings.Contains(w.Body.String(), `"code":-32022`) {
			t.Errorf("unsupported protocol accepted: %d %s", w.Code, w.Body)
		}
	}
	w := packageMCPRaw(t, s.Front(), p.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Content-Type", "text/plain")
	if w.Code != 415 {
		t.Errorf("wrong media type accepted: %d", w.Code)
	}
	for _, id := range []string{`"string-id"`, `0`, `-1`} {
		w := packageMCPRaw(t, s.Front(), p.APIKey, `{"jsonrpc":"2.0","id":`+id+`,"method":"tools/list"}`, "MCP-Protocol-Version", "2025-11-25")
		if w.Code != 200 {
			t.Errorf("valid id refused: %d %s", w.Code, w.Body)
		}
	}
	w = packageMCPRaw(t, s.Front(), p.APIKey, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != 202 {
		t.Fatalf("notification refused: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid metadata reached supplier: %d", calls.Load())
	}
}

type packageMCPByteReader struct{ io.Reader }

func (r packageMCPByteReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.Reader.Read(b)
}

func TestTokenPackMCPBodyHasFixedReadDeadline(t *testing.T) {
	s, p, calls := packageMCPSetup(t, packageMCPReply)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	r := httptest.NewRequest(http.MethodPost, packageMCPTestPath, nil)
	r.Body = io.NopCloser(packageMCPByteReader{strings.NewReader(body)})
	r.ContentLength = int64(len(body))
	r.RemoteAddr, r.Host = "127.0.0.1:50123", "127.0.0.1:3425"
	r.Header.Set("Authorization", "Bearer "+p.APIKey)
	r.Header.Set("Content-Type", "application/json")
	var deadlines []time.Time
	w := httptest.NewRecorder()
	s.Front().ServeHTTP(deadlineRecorder{ResponseWriter: w, deadlines: &deadlines}, r)
	if w.Code != 200 || len(deadlines) < 3 {
		t.Fatalf("body has no enforced deadline: status=%d deadlines=%d", w.Code, len(deadlines))
	}
	first := deadlines[0]
	if first.IsZero() || first.After(time.Now().Add(31*time.Second)) {
		t.Fatalf("invalid initial deadline: %v", first)
	}
	for _, deadline := range deadlines {
		if !deadline.IsZero() && deadline.After(first) {
			t.Fatalf("slow body extended total deadline: first=%v later=%v", first, deadline)
		}
	}
	if !deadlines[len(deadlines)-1].IsZero() || calls.Load() != 0 {
		t.Fatalf("deadline remained after body, or tool executed: %v calls=%d", deadlines, calls.Load())
	}
}

func TestTokenPackMCPRejectsCompressedBodyWithoutDecoder(t *testing.T) {
	s, p, calls := packageMCPSetup(t, packageMCPReply)
	for _, encoding := range []string{"gzip", "zstd", "br"} {
		w := packageMCPRaw(t, s.Front(), p.APIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Content-Encoding", encoding)
		if w.Code != 415 {
			t.Errorf("compressed MCP admitted: %s status=%d", encoding, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("compressed request reached provider: %d", calls.Load())
	}
}
