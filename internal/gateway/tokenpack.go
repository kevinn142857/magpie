package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/tokenpack"
	"github.com/yetone/magpie/internal/usage"
)

type tokenPackContext struct{}

const tokenPackBodyLimit int64 = 1 << 20

// A package's body is read by at most one request in this process. The
// durable allowance is reserved later, once its model has been validated.
var tokenPackReaders sync.Map

// packUsage receives the same normalized, billed usage as the gateway log.
// It contains counters only; upstream errors and request bodies never enter
// the package ledger. Each actual upstream attempt is included once.
type packUsage struct {
	sync.Mutex
	seen, unknown                        bool
	stream                               bool
	tokens                               int64
	input, output, cacheRead, cacheWrite int64
}

func recordTokenPackUsage(r *http.Request, rec usage.Record) {
	u, _ := r.Context().Value(tokenPackContext{}).(*packUsage)
	if u == nil {
		return
	}
	u.Lock()
	defer u.Unlock()
	u.seen = true
	if rec.IsRejected() {
		return
	}
	var n int64
	for _, count := range [...]int{rec.Input, rec.Output, rec.CacheRead, rec.CacheWrite} {
		v := int64(count)
		if v < 0 || v > tokenpack.MaxTokens || n > tokenpack.MaxTokens-v {
			u.unknown = true
			return
		}
		n += v
	}
	if u.tokens > tokenpack.MaxTokens-n {
		u.unknown = true
		return
	}
	u.tokens += n
	u.input += int64(rec.Input)
	u.output += int64(rec.Output)
	u.cacheRead += int64(rec.CacheRead)
	u.cacheWrite += int64(rec.CacheWrite)
	// Normalized records cannot distinguish an omitted count from an explicit
	// zero. Hold the allowance in either case instead of inventing free tokens.
	if rec.CacheUnknown || n-int64(rec.Output) == 0 || rec.Output == 0 || rec.Status >= 400 || rec.Error != "" || (u.stream && rec.Stop == "") {
		u.unknown = true
	}
}

type packWriter struct {
	http.ResponseWriter
	status int
	proto  provider.Protocol
	sniff  *usageSniffer
	failed bool
}

func (w *packWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *packWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.sniff == nil && strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.sniff = newSniffer(w.proto, "text/event-stream")
	}
	if w.sniff != nil {
		w.sniff.write(b)
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil || n != len(b) {
		w.failed = true
	}
	return n, err
}
func (w *packWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *packWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

// tokenPackLimited leaves ordinary gateway keys untouched. A package key
// always asks its independent ledger, even when its file has gone missing.
// Only text-generation APIs and their model catalog are exposed by a pack.
func (s *Server) tokenPackLimited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := access.Caller(r.Context())
		if !who.TokenPack {
			next.ServeHTTP(w, r)
			return
		}
		p, err := tokenpack.Get(who.KeyID)
		if err != nil {
			packRefused(w, r, "", tokenpack.ErrUnavailable)
			return
		}
		if !p.Enabled {
			packRefused(w, r, "", tokenpack.ErrDisabled)
			return
		}
		if !p.ExpiresAt.IsZero() && !time.Now().Before(p.ExpiresAt) {
			packRefused(w, r, "", tokenpack.ErrExpired)
			return
		}
		// The ledger's model list stays authoritative if another admin surface
		// changes the gateway key's copy of its allow-list.
		who.Models = p.Models
		r = r.WithContext(access.WithIdentity(r.Context(), who))
		if r.URL.Path == PackageMCPPath || (r.URL.Path == SearchMCPPath && p.MCPTotalCalls > 0) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && packCatalog(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || !packGeneration(r.URL.Path) {
			packRefused(w, r, "", tokenpack.ErrUnsupported)
			return
		}
		readID := tokenpack.Path() + "\x00" + p.ID
		if _, busy := tokenPackReaders.LoadOrStore(readID, true); busy {
			packRefused(w, r, "", tokenpack.ErrBusy)
			return
		}
		defer tokenPackReaders.Delete(readID)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		r = r.WithContext(ctx)
		// Account for the maximum decoded body before decompressing it, even
		// when its wire length is small or unknown. Ordinary keys are unchanged.
		if err := bodyRoom.take(ctx, tokenPackBodyLimit); err != nil {
			packRefused(w, r, "", tokenpack.ErrUnavailable)
			return
		}
		defer bodyRoom.give(ctx)
		limit := tokenPackBodyLimit
		if s.requestLimits.body > 0 {
			limit = min(limit, s.requestLimits.body)
		}
		// A compressed frame can demand a large decoder window before it
		// produces a bounded body. Package zstd decoders have their own 8 MiB
		// memory bound instead of the general gateway's 512 MiB allowance.
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "zstd") {
			original := r.Body
			decoder, err := zstd.NewReader(original, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20))
			if err != nil {
				recordPackRefusal(r, "", 400, tokenpack.ErrInvalid)
				writeError(w, protoOfPath(r.URL.Path), 400, "invalid compressed package request")
				return
			}
			defer decoder.Close()
			defer original.Close()
			r.Header = r.Header.Clone()
			r.Header.Del("Content-Encoding")
			r.Body = io.NopCloser(decoder)
			r.ContentLength = -1
		}
		input := &packWriter{ResponseWriter: w, proto: protoOfPath(r.URL.Path)}
		body, ok := s.readRequestBody(input, r, protoOfPath(r.URL.Path), limit)
		if !ok {
			recordPackRefusal(r, "", input.status, tokenpack.ErrInvalid)
			return
		}
		// Reserve before transforming the body so busy, exhausted and uncertain
		// allowances have distinct outcomes even when all their tokens are held.
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || fields == nil {
			recordPackRefusal(r, "", http.StatusBadRequest, tokenpack.ErrInvalid)
			writeError(w, protoOfPath(r.URL.Path), 400, "invalid JSON request")
			return
		}
		model := ""
		_ = json.Unmarshal(fields["model"], &model)
		if strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
			model, _, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":")
		}
		reservation, err := tokenpack.Reserve(p.ID, model, tokenPackEndpoint(r), time.Now())
		if err != nil {
			packRefused(w, r, model, err)
			return
		}
		u := &packUsage{}
		out := &packWriter{ResponseWriter: w, proto: protoOfPath(r.URL.Path)}
		finished := false
		defer func() {
			u.Lock()
			tokens, certain := u.tokens, finished && u.seen && !u.unknown && !out.failed
			if u.stream && (out.sniff == nil || !out.sniff.whole() || out.sniff.failed != "") {
				certain = false
			}
			report, _ := r.Context().Value(packageMCPReportKey{}).(*packageMCPReport)
			if report != nil {
				certain = certain && !report.OverLimit.Load()
				report.Usage = packageMCPUsage{Input: u.input, Output: u.output, CacheRead: u.cacheRead, CacheWrite: u.cacheWrite, Total: tokens}
			}
			u.Unlock()
			entry, err := tokenpack.Complete(reservation.ID, tokens, out.status, certain)
			if err != nil {
				// A failed settlement keeps its original durable reservation.
				log.Print("token package settlement failed; reservation retained")
			}
			if report != nil {
				report.Usage.Complete = err == nil && entry.Outcome == "settled"
			}
		}()
		prepared, stream, err := packBody(body, fields, r.URL.Path, reservation.Tokens)
		if err != nil {
			u.seen = true // rejected locally before any provider was contacted
			finished = true
			writeError(out, protoOfPath(r.URL.Path), http.StatusBadRequest, err.Error())
			return
		}
		u.stream = stream
		r = r.WithContext(context.WithValue(r.Context(), tokenPackContext{}, u))
		r.Header = r.Header.Clone()
		r.Header.Del("Content-Encoding")
		r.Body = io.NopCloser(bytes.NewReader(prepared))
		r.ContentLength = int64(len(prepared))
		next.ServeHTTP(out, r)
		finished = true
	})
}

func packCatalog(path string) bool {
	return path == "/v1/models" || path == "/models" || strings.HasPrefix(path, "/v1/models/") || path == "/v1beta/models"
}

func packGeneration(path string) bool {
	switch path {
	case "/v1/chat/completions", "/chat/completions", "/v1/responses", "/responses", "/v1/messages", "/messages":
		return true
	}
	return strings.HasPrefix(path, "/v1beta/models/") && (strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent"))
}

// packBody sets a conservative output bound, using twice the JSON byte count
// plus framing room as an input estimate. This is intentionally not advertised
// as exact tokenization for arbitrary models; actual reported usage is billed.
func packBody(original []byte, fields map[string]json.RawMessage, path string, remaining int64) ([]byte, bool, error) {
	var shape any
	if json.Unmarshal(original, &shape) != nil {
		return nil, false, errors.New("invalid JSON request")
	}
	if packMultimodal(shape) {
		return nil, false, errors.New("token packages currently accept text inputs only")
	}
	if packServerTools(shape) {
		return nil, false, errors.New("token packages do not include server-side search or paid tools")
	}
	room := remaining - int64(len(original))*2 - 128
	if room <= 0 {
		return nil, false, errors.New("remaining allowance is too small for this request's conservative input estimate")
	}
	var stream bool
	if b, ok := fields["stream"]; ok && json.Unmarshal(b, &stream) != nil {
		return nil, false, errors.New("stream must be a boolean")
	}
	stream = stream || strings.HasSuffix(path, ":streamGenerateContent")
	var n int64
	if b, ok := fields["n"]; ok && (json.Unmarshal(b, &n) != nil || n != 1) {
		return nil, false, errors.New("token packages allow one completion per request")
	}
	keys := []string{"max_tokens", "max_completion_tokens"}
	output := "max_completion_tokens"
	if strings.HasSuffix(path, "/messages") {
		keys = []string{"max_tokens"}
		output = "max_tokens"
	}
	if strings.HasSuffix(path, "/responses") {
		keys = []string{"max_output_tokens"}
		output = "max_output_tokens"
	}
	target := fields
	if strings.HasPrefix(path, "/v1beta/") {
		target = map[string]json.RawMessage{}
		if b, ok := fields["generationConfig"]; ok && json.Unmarshal(b, &target) != nil {
			return nil, false, errors.New("invalid generationConfig")
		}
		if target == nil {
			target = map[string]json.RawMessage{}
		}
		keys = []string{"maxOutputTokens"}
		output = "maxOutputTokens"
		if b, ok := target["candidateCount"]; ok && (json.Unmarshal(b, &n) != nil || n != 1) {
			return nil, false, errors.New("token packages allow one candidate per request")
		}
	}
	cap := min(room, int64(4096))
	asked := false
	for _, key := range keys {
		if b, ok := target[key]; ok {
			if json.Unmarshal(b, &n) != nil || n <= 0 || n > tokenpack.MaxTokens {
				return nil, false, errors.New("output token limit must be a positive integer")
			}
			if !asked {
				cap = min(room, n)
				asked = true
			} else {
				cap = min(cap, n)
			}
		}
		delete(target, key)
	}
	target[output] = json.RawMessage(strconv.FormatInt(cap, 10))
	if strings.HasPrefix(path, "/v1beta/") {
		b, _ := json.Marshal(target)
		fields["generationConfig"] = b
	}
	prepared, err := json.Marshal(fields)
	return prepared, stream, err
}

// Client function/custom declarations return calls to the client. Every other
// tool may spend a vendor's separate allowance or invoke an administrator's
// configured service, neither of which is represented by a token-only package.
func packServerTools(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for key, value := range x {
			if key == "parameters" || key == "input_schema" || key == "parametersJsonSchema" {
				continue // JSON Schema properties are data, not active tools.
			}
			switch key {
			case "web_search_options", "webSearchOptions", "search_parameters", "searchParameters":
				if value != nil {
					return true
				}
			case "enable_search", "enableSearch":
				if value != nil && value != false {
					return true
				}
			case "plugins":
				if list, ok := value.([]any); value != nil && (!ok || len(list) != 0) {
					return true
				}
			case "tools", "additional_tools":
				if value == nil {
					continue
				}
				tools, ok := value.([]any)
				if !ok {
					return true
				}
				for _, raw := range tools {
					tool, ok := raw.(map[string]any)
					if !ok {
						return true
					}
					if kind, typed := tool["type"]; typed {
						if kind != "function" && kind != "custom" {
							return true
						}
						continue
					}
					if name, ok := tool["name"].(string); ok && name != "" && tool["input_schema"] != nil {
						continue // Anthropic's ordinary client tool.
					}
					if len(tool) == 1 && (tool["functionDeclarations"] != nil || tool["function_declarations"] != nil) {
						continue // Gemini's ordinary client functions.
					}
					return true
				}
			}
			if packServerTools(value) {
				return true
			}
		}
	case []any:
		for _, value := range x {
			if packServerTools(value) {
				return true
			}
		}
	}
	return false
}

func packMultimodal(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for key, value := range x {
			// Function argument schemas describe data; a property named
			// modalities or file_data is not an active media request.
			if key == "parameters" || key == "input_schema" || key == "parametersJsonSchema" {
				continue
			}
			if key == "inlineData" || key == "fileData" || key == "inline_data" || key == "file_data" || key == "image_url" || key == "input_audio" {
				return true
			}
			if key == "modalities" {
				values, ok := value.([]any)
				if !ok {
					return true
				}
				for _, modality := range values {
					if modality != "text" {
						return true
					}
				}
			}
			if key == "type" {
				switch value {
				case "image", "image_url", "input_image", "input_audio", "audio", "input_file", "file", "video", "video_url", "input_video", "document", "image_generation":
					return true
				}
			}
			if packMultimodal(value) {
				return true
			}
		}
	case []any:
		for _, value := range x {
			if packMultimodal(value) {
				return true
			}
		}
	}
	return false
}

func packRefused(w http.ResponseWriter, r *http.Request, model string, err error) {
	status, message := http.StatusServiceUnavailable, "package ledger unavailable; no request was sent"
	switch {
	case errors.Is(err, tokenpack.ErrBusy):
		status, message = http.StatusConflict, "package has a request in flight; wait for it to finish"
	case errors.Is(err, tokenpack.ErrUncertain):
		status, message = http.StatusLocked, "package usage is uncertain; administrator reconciliation required"
	case errors.Is(err, tokenpack.ErrExhausted):
		status, message = http.StatusTooManyRequests, "package token allowance exhausted"
	case errors.Is(err, tokenpack.ErrDisabled):
		status, message = http.StatusForbidden, "package is disabled"
	case errors.Is(err, tokenpack.ErrExpired):
		status, message = http.StatusGone, "package has expired"
	case errors.Is(err, tokenpack.ErrModel):
		status, message = http.StatusForbidden, "model is outside this package"
	case errors.Is(err, tokenpack.ErrUnsupported):
		status, message = http.StatusForbidden, "token packages support approved model APIs and scoped MCP only"
	}
	recordPackRefusal(r, model, status, err)
	w.Header().Set("X-Should-Retry", "false")
	writeError(w, protoOfPath(r.URL.Path), status, message)
}

func recordPackRefusal(r *http.Request, model string, status int, reason error) {
	id := access.Caller(r.Context()).KeyID
	if err := tokenpack.RecordRefusal(id, model, tokenPackEndpoint(r), status, reason); err != nil {
		log.Print("token package refusal could not be recorded; no request was sent")
	}
}

func tokenPackEndpoint(r *http.Request) string {
	if report, _ := r.Context().Value(packageMCPReportKey{}).(*packageMCPReport); report != nil {
		return PackageMCPPath
	}
	return r.URL.Path
}
