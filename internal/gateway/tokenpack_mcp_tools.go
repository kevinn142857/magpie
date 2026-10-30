package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/tokenpack"
)

const packageMCPToolTimeout = 30 * time.Second
const packageMCPToolWorkingSet int64 = 32 << 20

type packageMCPToolContextKey struct{}
type packageMCPFetchClientKey struct{}
type packageMCPSearchAPIsKey struct{}

func packageMCPSearchAPIs(ctx context.Context) []provider.SearchAPI {
	if apis, ok := ctx.Value(packageMCPSearchAPIsKey{}).([]provider.SearchAPI); ok {
		return apis
	}
	return provider.SearchAPIs()
}

func packageMCPToolActive(ctx context.Context) bool {
	on, _ := ctx.Value(packageMCPToolContextKey{}).(bool)
	return on
}

// The normal web-fetch path retains its original client and network policy.
func packageMCPFetchClient(ctx context.Context) *http.Client {
	if client, _ := ctx.Value(packageMCPFetchClientKey{}).(*http.Client); client != nil {
		return client
	}
	return webFetchClient
}

type packageMCPToolTransport struct{ base http.RoundTripper }

func (t packageMCPToolTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(r)
	if err == nil && res.Body != nil {
		res.Body = &packageMCPUpstreamBody{ReadCloser: res.Body, remaining: packageMCPReplyLimit, report: &packageMCPReport{}}
	}
	return res, err
}

func packageMCPCallTools() []any {
	return []any{
		map[string]any{"name": "web_search", "description": "Search with the configured search APIs, without using another model. One accepted tool execution consumes one call, including failures. An unconfigured search or invalid arguments consume none.",
			"inputSchema": json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":8192}},"required":["query"],"additionalProperties":false}`)},
		map[string]any{"name": "web_fetch", "description": "Fetch a public HTTP(S) page as text. Local files and private network targets are unavailable. One accepted execution consumes one call, including network and policy failures.",
			"inputSchema": json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","minLength":1,"maxLength":8192},"max_chars":{"type":"integer","minimum":1,"maximum":100000,"default":20000},"offset":{"type":"integer","minimum":0,"default":0}},"required":["url"],"additionalProperties":false}`)},
	}
}

func packageMCPToolArgs(raw json.RawMessage, out any) bool {
	if !packageMCPDecode(raw, out) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}

func packageMCPFetchArgs(raw json.RawMessage) (fetchArgs, bool) {
	var in struct {
		URL      string `json:"url"`
		MaxChars *int   `json:"max_chars"`
		Offset   *int   `json:"offset"`
	}
	if !packageMCPToolArgs(raw, &in) {
		return fetchArgs{}, false
	}
	in.URL = strings.TrimSpace(in.URL)
	u, err := url.Parse(in.URL)
	if err != nil || len(in.URL) > 8192 || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsFunc(in.URL, unicode.IsSpace) {
		return fetchArgs{}, false
	}
	a := fetchArgs{URL: in.URL, MaxChars: webFetchChars}
	if in.MaxChars != nil {
		if *in.MaxChars < 1 || *in.MaxChars > webFetchMaxChars {
			return a, false
		}
		a.MaxChars = *in.MaxChars
	}
	if in.Offset != nil {
		if *in.Offset < 0 {
			return a, false
		}
		a.Offset = *in.Offset
	}
	return a, true
}

func packageMCPCallRefused(err error) (int, string) {
	switch {
	case errors.Is(err, tokenpack.ErrMCPExhausted):
		return http.StatusTooManyRequests, "The package's MCP call allowance is exhausted."
	case errors.Is(err, tokenpack.ErrDisabled):
		return http.StatusForbidden, "The package is disabled."
	case errors.Is(err, tokenpack.ErrExpired):
		return http.StatusGone, "The package has expired."
	}
	return http.StatusServiceUnavailable, "The package call ledger is unavailable."
}

func packageMCPToolResult(id, tool, text string, status int, charged int64) map[string]any {
	structured := map[string]any{"tool": tool, "text": text, "statusCode": status, "usage": map[string]int64{"calls": charged}}
	if p, err := tokenpack.Get(id); err == nil {
		structured["quota"] = packageQuota(p)
	}
	return packageMCPResult(text, structured, status >= 400)
}

func (s *Server) packageMCPCallTool(r *http.Request, tool string, raw json.RawMessage) (any, map[string]any) {
	var query string
	var fetch fetchArgs
	if tool == "web_search" {
		var in struct {
			Query string `json:"query"`
		}
		if !packageMCPToolArgs(raw, &in) {
			return packageMCPInvalid()
		}
		query = strings.TrimSpace(in.Query)
		if query == "" || len(query) > 8192 {
			return packageMCPInvalid()
		}
	} else {
		var ok bool
		fetch, ok = packageMCPFetchArgs(raw)
		if !ok {
			return packageMCPInvalid()
		}
	}
	id := access.Caller(r.Context()).KeyID
	p, err := tokenpack.Get(id)
	if err != nil {
		return packageMCPToolResult(id, tool, "The package call ledger is unavailable.", 503, 0), nil
	}
	if p.MCPTotalCalls <= 0 {
		return packageMCPToolResult(id, tool, "This package does not include web tools.", 403, 0), nil
	}
	var searchAPIs []provider.SearchAPI
	if tool == "web_search" {
		stored, err := provider.StoredSearchAPIsChecked()
		if err != nil {
			return packageMCPToolResult(id, tool, "The search API configuration is unavailable.", 503, 0), nil
		}
		for _, api := range stored {
			if api.Ready() && !api.Off {
				searchAPIs = append(searchAPIs, api)
			}
		}
		if len(searchAPIs) == 0 {
			return packageMCPToolResult(id, tool, "No search API is configured. Ask the administrator to configure one.", 503, 0), nil
		}
	}
	// Queue before charging. The same deadline covers waiting, execution and
	// writing the bounded RPC result, so queued work cannot spend calls early.
	ctx, cancel := context.WithTimeout(netproxy.With(r.Context(), ""), packageMCPToolTimeout)
	if scope, _ := r.Context().Value(packageMCPResourceKey{}).(*packageMCPResource); scope != nil {
		scope.ctx, scope.cancel = ctx, cancel
		until, _ := ctx.Deadline()
		if err := scope.controller.SetWriteDeadline(until); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return packageMCPToolResult(id, tool, "The tool deadline could not be enforced; no call was charged.", 503, 0), nil
		}
	} else {
		defer cancel()
		defer bodyRoom.give(ctx)
	}
	if ctx.Err() != nil {
		return packageMCPToolResult(id, tool, "The tool deadline ended before execution; no call was charged.", 504, 0), nil
	}
	if err := bodyRoom.take(ctx, packageMCPToolWorkingSet); err != nil || ctx.Err() != nil {
		return packageMCPToolResult(id, tool, "The tool could not obtain working memory before its deadline; no call was charged.", 503, 0), nil
	}
	requestID, err := tokenpack.ReserveMCPCall(id, tool, time.Now())
	if err != nil {
		status, message := packageMCPCallRefused(err)
		return packageMCPToolResult(id, tool, message, status, 0), nil
	}
	status, completed := http.StatusInternalServerError, false
	defer func() {
		if !completed {
			if err := tokenpack.CompleteMCPCall(requestID, status); err != nil {
				log.Print("package MCP completion could not be persisted; accepted call remains charged")
			}
		}
	}()
	ctx = context.WithValue(ctx, packageMCPToolContextKey{}, true)
	var text string
	if tool == "web_search" {
		// This path intentionally never calls webSearch, modelSearch or
		// ownSearch: no administrator-selected model can spend outside the
		// package's model allow-list, and no model tokens are fabricated.
		ctx = context.WithValue(ctx, packageMCPSearchAPIsKey{}, searchAPIs)
		text, _, err = s.apiSearch(ctx, query)
		text = packageMCPSearchText(text, searchAPIs)
		if err == nil && strings.TrimSpace(text) == "" {
			err = errors.New("empty search response")
		}
	} else {
		client := *webFetchClient
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		client.Transport = packageMCPToolTransport{base: transport}
		client.Timeout = packageMCPToolTimeout
		ctx = context.WithValue(ctx, packageMCPFetchClientKey{}, &client)
		u, _ := url.Parse(fetch.URL)
		var title, page, final string
		title, page, final, err = fetchPage(ctx, u)
		if err == nil {
			text = fetchPart(final, title, page, fetch.Offset, fetch.MaxChars)
		}
	}
	status = http.StatusOK
	if err != nil {
		status = http.StatusBadGateway
		if ctx.Err() != nil {
			status = http.StatusGatewayTimeout
		}
		text = "The web tool could not complete this request. Its accepted call remains charged."
	}
	if err := tokenpack.CompleteMCPCall(requestID, status); err != nil {
		status = http.StatusServiceUnavailable
		text = "The tool was accepted and charged, but its completion could not be recorded."
	} else {
		completed = true
	}
	return packageMCPToolResult(id, tool, text, status, 1), nil
}

// A search API may echo its credential in otherwise successful page text.
func packageMCPSearchText(text string, apis []provider.SearchAPI) string {
	for _, api := range apis {
		for _, key := range api.Keys() {
			if key != "" {
				text = strings.ReplaceAll(text, key, "[redacted]")
			}
		}
	}
	return text
}
