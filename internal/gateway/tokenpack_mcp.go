package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/tokenpack"
)

// PackageMCPPath serves package-scoped models and separately metered web tools.
// It never exposes plugins, local files, or the administrator's MCP sign-ins.
const PackageMCPPath = "/mcp/magpie/package"

const (
	packageMCPBodyLimit   = 64 << 10
	packageMCPReplyLimit  = 4 << 20
	packageMCPTimeout     = 60 * time.Second
	packageMCPBodyTimeout = 30 * time.Second
)

var packageMCPVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

type packageMCPQuota struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Models            []string   `json:"models"`
	TotalTokens       int64      `json:"totalTokens"`
	UsedTokens        int64      `json:"usedTokens"`
	ReservedTokens    int64      `json:"reservedTokens"`
	RemainingTokens   int64      `json:"remainingTokens"`
	Enabled           bool       `json:"enabled"`
	ExpiresAt         *time.Time `json:"expiresAt"`
	MCPTotalCalls     int64      `json:"mcpTotalCalls"`
	MCPUsedCalls      int64      `json:"mcpUsedCalls"`
	MCPRemainingCalls int64      `json:"mcpRemainingCalls"`
}

func packageQuota(p tokenpack.Pack) packageMCPQuota {
	q := packageMCPQuota{ID: p.ID, Name: p.Name, Models: slices.Clone(p.Models), TotalTokens: p.TotalTokens, UsedTokens: p.UsedTokens,
		ReservedTokens: p.ReservedTokens, RemainingTokens: p.RemainingTokens, Enabled: p.Enabled,
		MCPTotalCalls: p.MCPTotalCalls, MCPUsedCalls: p.MCPUsedCalls, MCPRemainingCalls: p.MCPRemainingCalls}
	if !p.ExpiresAt.IsZero() {
		q.ExpiresAt = &p.ExpiresAt
	}
	return q
}

type packageMCPUsage struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	CacheRead  int64 `json:"cacheReadTokens"`
	CacheWrite int64 `json:"cacheWriteTokens"`
	Total      int64 `json:"totalTokens"`
	Complete   bool  `json:"complete"`
}

// Only an in-process caller can install this report. The accounting middleware
// fills it after durable settlement, including every normalized upstream attempt.
type packageMCPReportKey struct{}
type packageMCPReport struct {
	Usage     packageMCPUsage
	OverLimit atomic.Bool
}

type packageMCPResourceKey struct{}
type packageMCPResource struct {
	controller *http.ResponseController
	ctx        context.Context
	cancel     context.CancelFunc
}

func (scope *packageMCPResource) finish() {
	if scope.cancel != nil {
		// Hold the working set through JSON encoding and the HTTP write.
		bodyRoom.give(scope.ctx)
		scope.cancel()
		_ = scope.controller.SetWriteDeadline(time.Time{})
	}
}

// Limit the vendor body before protocol adapters or chatWhole can buffer it.
// The one-byte probe distinguishes an exact-limit EOF from a truncated reply.
func packageMCPLimitResponse(ctx context.Context, res *http.Response) {
	report, _ := ctx.Value(packageMCPReportKey{}).(*packageMCPReport)
	if report != nil && res != nil && res.Body != nil {
		res.Body = &packageMCPUpstreamBody{ReadCloser: res.Body, remaining: packageMCPReplyLimit, report: report}
	}
}

type packageMCPUpstreamBody struct {
	io.ReadCloser
	remaining int64
	report    *packageMCPReport
}

func (r *packageMCPUpstreamBody) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.ReadCloser.Read(probe[:])
		if n > 0 {
			r.report.OverLimit.Store(true)
			return 0, errors.New("package MCP upstream response exceeded its limit")
		}
		return 0, err
	}
	if int64(len(b)) > r.remaining {
		b = b[:r.remaining]
	}
	n, err := r.ReadCloser.Read(b)
	r.remaining -= int64(n)
	return n, err
}

func (s *Server) packageMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !access.Caller(r.Context()).TokenPack {
		w.Header().Set("WWW-Authenticate", `Bearer realm="magpie-package"`)
		writeError(w, provider.Chat, http.StatusUnauthorized, "A token package API key is required.")
		return
	}
	if r.Method == http.MethodPost {
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
			packageMCPEnvelopeError(w, http.StatusUnsupportedMediaType, "Use Content-Type: application/json.")
			return
		}
		if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
			packageMCPEnvelopeError(w, http.StatusUnsupportedMediaType, "Package MCP accepts uncompressed JSON only.")
			return
		}
		if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !slices.Contains(packageMCPVersions, version) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{
				"code": -32022, "message": "Unsupported MCP protocol version.",
				"data": map[string]any{"supported": packageMCPVersions, "requested": version},
			}})
			return
		}
		// The shared legacy MCP framing accepts batches and retains every
		// result. A package admits one bounded operation per HTTP request.
		if r.Body != nil {
			defer r.Body.Close()
		}
		within := packageMCPBodyTimeout
		if s.requestLimits.readTimeout > 0 {
			within = min(within, s.requestLimits.readTimeout)
		}
		deadline := packageMCPReadDeadline{ResponseWriter: w, until: time.Now().Add(within)}
		body, status, err := readBoundedRequestBody(deadline, r, requestLimits{body: packageMCPBodyLimit, readTimeout: within})
		if err != nil {
			packageMCPEnvelopeError(w, status, "The package MCP body could not be read within its size and time limits.")
			return
		}
		if !packageMCPSingleRequest(body) {
			packageMCPEnvelopeError(w, http.StatusBadRequest, "Send one JSON-RPC 2.0 request with a string or integer id; batches are not supported.")
			return
		}
		r = r.Clone(r.Context())
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	}
	scope := &packageMCPResource{controller: http.NewResponseController(w)}
	r = r.WithContext(context.WithValue(r.Context(), packageMCPResourceKey{}, scope))
	defer scope.finish()
	serveMCP(w, r, "this token package", packageMCPBodyLimit, s.packageMCPCall)
}

// readBoundedRequestBody renews an idle socket deadline on every read. This
// wrapper keeps those renewals within the package's absolute body-read budget.
type packageMCPReadDeadline struct {
	http.ResponseWriter
	until time.Time
}

func (w packageMCPReadDeadline) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w packageMCPReadDeadline) SetReadDeadline(at time.Time) error {
	if !at.IsZero() && at.After(w.until) {
		at = w.until
	}
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(at)
}

func packageMCPEnvelopeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": message}})
}

func packageMCPSingleRequest(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return false
	}
	var request struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		ID      json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &request) != nil || request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
		return false
	}
	id := bytes.TrimSpace(request.ID)
	if len(id) == 0 {
		return strings.HasPrefix(request.Method, "notifications/")
	}
	if id[0] == '"' {
		var value string
		return json.Unmarshal(id, &value) == nil
	}
	_, err := strconv.ParseInt(string(id), 10, 64)
	return err == nil
}

func packageMCPResult(text string, structured any, failed bool) map[string]any {
	result := toolResult(text, failed)
	if structured != nil {
		result["structuredContent"] = structured
	}
	// Text and structuredContent can both contain the answer. Bound their
	// combined encoding, reserving room for the caller's bounded JSON-RPC id.
	if b, err := json.Marshal(result); err != nil || len(b) > packageMCPReplyLimit-packageMCPBodyLimit-1024 {
		return toolResult("The generated result exceeds the package MCP response limit. Request a shorter answer.", true)
	}
	return result
}

func packageMCPJSONResult(v any) map[string]any {
	b, _ := json.Marshal(v)
	return packageMCPResult(string(b), v, false)
}

func packageMCPInvalid() (any, map[string]any) {
	return nil, map[string]any{"code": -32602, "message": "Invalid tool arguments. Use the declared schema."}
}

func packageMCPDecode(raw json.RawMessage, out any) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = json.RawMessage(`{}`)
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil && d.Decode(new(any)) == io.EOF
}

func packageMCPTools(calls bool) []any {
	empty := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	tools := []any{
		map[string]any{"name": "get_quota", "description": "Read this package's lifetime token balance. Reserved tokens remain unavailable while usage is pending or uncertain. A null expiresAt means no expiry.", "inputSchema": empty},
		map[string]any{"name": "list_models", "description": "List currently available model references allowed by this package.", "inputSchema": empty},
		map[string]any{"name": "generate_text", "description": "Generate plain text with an allowed model. The actual reported input, output and cache tokens consume this package. Incomplete usage keeps its allowance reserved for administrator review.",
			"inputSchema": json.RawMessage(`{"type":"object","properties":{"model":{"type":"string","minLength":1,"description":"An allowed model reference from list_models."},"prompt":{"type":"string","minLength":1},"system":{"type":"string"},"maxTokens":{"type":"integer","minimum":1,"default":4096}},"required":["model","prompt"],"additionalProperties":false}`)},
	}
	if calls {
		tools = append(tools, packageMCPCallTools()...)
	}
	return tools
}

func (s *Server) packageMCPCall(r *http.Request, m mcpMessage) (any, map[string]any) {
	switch m.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &params)
		version := packageMCPVersions[0]
		if slices.Contains(packageMCPVersions, params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]any{"name": "magpie-package", "title": "Magpie Token Package", "version": Version},
			"instructions": "Use get_quota to inspect balances and list_models to choose an allowed model. generate_text consumes tokens. web_search and web_fetch are available only with a separate tool-call allowance; every accepted execution consumes one call even when it fails. Images, plugins, local files and administrator tools are not exposed."}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		p, err := tokenpack.Get(access.Caller(r.Context()).KeyID)
		if err != nil {
			return nil, map[string]any{"code": -32603, "message": "The package ledger is unavailable."}
		}
		return map[string]any{"tools": packageMCPTools(p.MCPTotalCalls > 0)}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &params) != nil {
			return packageMCPInvalid()
		}
		if params.Name == "generate_text" {
			return s.packageMCPGenerate(r, params.Arguments)
		}
		if params.Name == "web_search" || params.Name == "web_fetch" {
			return s.packageMCPCallTool(r, params.Name, params.Arguments)
		}
		if params.Name != "get_quota" && params.Name != "list_models" {
			return nil, map[string]any{"code": -32602, "message": "Unknown package tool."}
		}
		if !packageMCPDecode(params.Arguments, new(struct{})) {
			return packageMCPInvalid()
		}
		if params.Name == "list_models" {
			models := []string{}
			for _, e := range keyAllowed(r, provider.Catalog()) {
				ref := e.Provider.ID + "/" + e.Model
				if e.Group != "" {
					ref = provider.GroupPrefix + e.Group
				}
				if !slices.Contains(models, ref) {
					models = append(models, ref)
				}
			}
			return packageMCPJSONResult(map[string]any{"models": models}), nil
		}
		p, err := tokenpack.Get(access.Caller(r.Context()).KeyID)
		if err != nil {
			return packageMCPResult("The package ledger is unavailable.", nil, true), nil
		}
		return packageMCPJSONResult(map[string]any{"quota": packageQuota(p)}), nil
	}
	return nil, map[string]any{"code": -32601, "message": "Method not supported by the package MCP."}
}

func (s *Server) packageMCPGenerate(r *http.Request, raw json.RawMessage) (any, map[string]any) {
	var args struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		System    string `json:"system"`
		MaxTokens *int64 `json:"maxTokens"`
	}
	if !packageMCPDecode(raw, &args) || strings.TrimSpace(args.Model) == "" || len(args.Model) > 300 || strings.TrimSpace(args.Prompt) == "" {
		return packageMCPInvalid()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, field := range fields {
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return packageMCPInvalid()
		}
	}
	maxTokens := int64(4096)
	if args.MaxTokens != nil {
		maxTokens = *args.MaxTokens
	}
	if maxTokens <= 0 || maxTokens > tokenpack.MaxTokens {
		return packageMCPInvalid()
	}
	args.Model = strings.TrimSpace(args.Model)
	messages := []map[string]string{}
	if args.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": args.System})
	}
	messages = append(messages, map[string]string{"role": "user", "content": args.Prompt})
	body, err := json.Marshal(map[string]any{"model": args.Model, "messages": messages, "max_tokens": maxTokens, "stream": false})
	if err != nil {
		return packageMCPInvalid()
	}
	ctx, cancel := context.WithTimeout(r.Context(), packageMCPTimeout)
	defer cancel()
	report := &packageMCPReport{}
	ctx = context.WithValue(ctx, packageMCPReportKey{}, report)
	in := inProcess(r, ctx, http.MethodPost, "/v1/chat/completions", body)
	// MCP metadata and client headers do not turn this into an agent-specific
	// auxiliary request. The package model and ordinary gateway guards hold it.
	in.Header.Set("User-Agent", "magpie-package-mcp/1")
	rec := &packageMCPRecorder{header: make(http.Header)}
	s.tokenPackLimited(s.handle(provider.Chat)).ServeHTTP(rec, in)
	if rec.failed || report.OverLimit.Load() || rec.status >= 400 || ctx.Err() != nil {
		return packageMCPResult("Text generation failed or the package cannot admit this request. Check its balance and request logs.", map[string]any{"statusCode": rec.status}, true), nil
	}
	var reply struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(rec.body.Bytes(), &reply) != nil || (len(reply.Error) != 0 && string(reply.Error) != "null") || len(reply.Choices) == 0 || strings.TrimSpace(reply.Choices[0].Message.Content) == "" {
		return packageMCPResult("The model did not return a usable plain text answer.", nil, true), nil
	}
	p, err := tokenpack.Get(access.Caller(r.Context()).KeyID)
	if err != nil {
		return packageMCPResult("The package ledger is unavailable after generation. Check its request logs.", nil, true), nil
	}
	text := reply.Choices[0].Message.Content
	structured := map[string]any{"model": args.Model, "text": text, "usage": report.Usage, "quota": packageQuota(p)}
	if !report.Usage.Complete {
		return packageMCPResult(text+"\n\nUsage is incomplete; the allowance remains reserved for administrator review.", structured, false), nil
	}
	return packageMCPResult(text, structured, false), nil
}

// A model may ignore its requested output limit. Stop retaining its reply at a
// fixed bound and propagate the failure to packWriter, which retains allowance.
type packageMCPRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	failed bool
}

func (w *packageMCPRecorder) Header() http.Header { return w.header }
func (w *packageMCPRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *packageMCPRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(b) > packageMCPReplyLimit-w.body.Len() {
		w.failed = true
		return 0, errors.New("package MCP response exceeded its limit")
	}
	return w.body.Write(b)
}
