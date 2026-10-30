package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/tokenpack"
	"github.com/yetone/magpie/internal/usage"
)

func packRequest(h http.Handler, key, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestTokenPackActualGatewayStopsExhaustedAndPersistsBalance(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	var calls atomic.Int64
	serveOn(t, "plan", "upstream-secret", []string{"m1", "other"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n, _ := body["max_completion_tokens"].(float64)
		if n == 0 {
			n, _ = body["max_tokens"].(float64)
		}
		if n <= 0 || n > 10_000 {
			t.Error("output limit was not constrained")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"done","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9950,"completion_tokens":50}}`)
	}))
	p, err := tokenpack.Create("One", []string{"plan/m1"}, 10_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	h := New().Front()
	body := `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}],"max_tokens":1000000}`
	if w := packRequest(h, p.APIKey, "/v1/chat/completions", body); w.Code != 200 {
		t.Fatalf("first request: %d %s", w.Code, w.Body)
	}
	if err := os.Remove(usage.Path()); err != nil {
		t.Fatal(err)
	}
	// A new gateway and a deleted usage log cannot recharge a lifetime pack.
	w := packRequest(New().Front(), p.APIKey, "/v1/chat/completions", body)
	if w.Code != 429 || calls.Load() != 1 {
		t.Fatalf("exhausted package reached upstream: %d, calls=%d, %s", w.Code, calls.Load(), w.Body)
	}
	p, err = tokenpack.Get(p.ID)
	if err != nil || p.UsedTokens != 10_000 || p.ReservedTokens != 0 {
		t.Fatalf("balance: %+v %v", p, err)
	}
	entries, err := tokenpack.Logs(p.ID, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("missing exhausted rejection log: %+v %v", entries, err)
	}
	found := false
	for _, e := range entries {
		if e.StatusCode == 429 && e.Outcome == "rejected" {
			found = true
		}
	}
	if !found {
		t.Fatal("exhaustion rejection was not logged")
	}
}

func TestTokenPackTextProtocolsApplyCapsAndSettle(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, reply string
		proto                   provider.Protocol
	}{
		{"responses", "/v1/responses", `{"model":"plan/m1","input":"hi","max_output_tokens":1000000}`, `{"id":"resp_test","object":"response","status":"completed","output":[{"type":"message","id":"msg_test","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":300,"output_tokens":50,"total_tokens":350}}`, provider.Responses},
		{"anthropic", "/v1/messages", `{"model":"plan/m1","max_tokens":1000000,"messages":[{"role":"user","content":"hi"}]}`, `{"id":"msg_test","type":"message","role":"assistant","model":"m1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":300,"output_tokens":50}}`, provider.Anthropic},
		{"gemini", "/v1beta/models/plan/m1:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":1000000}}`, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":50,"totalTokenCount":350}}`, provider.Gemini},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				var n float64
				switch tc.proto {
				case provider.Responses:
					n, _ = body["max_output_tokens"].(float64)
				case provider.Anthropic:
					n, _ = body["max_tokens"].(float64)
				case provider.Gemini:
					config, _ := body["generationConfig"].(map[string]any)
					n, _ = config["maxOutputTokens"].(float64)
				}
				if n <= 0 || n >= 10_000 {
					t.Errorf("output cap not applied: %v", body)
				}
				if tc.proto == provider.Gemini {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: "+tc.reply+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.reply)
			}))
			defer up.Close()
			pr := provider.Provider{ID: "plan", Name: "Plan", Key: "upstream-secret", Models: []string{"m1"}}
			switch tc.proto {
			case provider.Responses:
				pr.Responses = up.URL + "/v1"
			case provider.Anthropic:
				pr.Anthropic = up.URL
			case provider.Gemini:
				pr.Gemini = up.URL + "/v1beta"
			}
			if err := provider.Save(pr); err != nil {
				t.Fatal(err)
			}
			p, err := tokenpack.Create("Protocol", []string{"plan/m1"}, 10_000, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			w := packRequest(New().Front(), p.APIKey, tc.path, tc.body)
			if w.Code != 200 {
				t.Fatalf("request: %d %s", w.Code, w.Body)
			}
			p, err = tokenpack.Get(p.ID)
			if err != nil || p.UsedTokens != 350 || p.ReservedTokens != 0 {
				t.Fatalf("protocol balance: %+v %v", p, err)
			}
		})
	}
}

func TestTokenPackStreamWithUsageButWithoutTerminalMarkerRemainsUncertain(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	serveOn(t, "plan", "upstream-secret", []string{"m1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"partial\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":50}}\n\n")
	}))
	p, err := tokenpack.Create("Stream missing terminal", []string{"plan/m1"}, 10_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	packRequest(New().Front(), p.APIKey, "/v1/chat/completions", `{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	p, err = tokenpack.Get(p.ID)
	if err != nil || p.ReservedTokens != 10_000 || p.UsedTokens != 0 {
		t.Fatalf("missing stream terminator released allowance: %+v %v", p, err)
	}
}

func TestTokenPackGatewayDisabledExpiredModelsAndMissingLedger(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	calls := limitedUpstream(t, 0)
	p, err := tokenpack.Create("Controls", []string{"plan/m1"}, 10_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	h := New().Front()
	body := `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`
	if _, err := tokenpack.Update(p.ID, 10_000, false); err != nil {
		t.Fatal(err)
	}
	if w := packRequest(h, p.APIKey, "/v1/chat/completions", body); w.Code != 403 {
		t.Fatalf("disabled: %d %s", w.Code, w.Body)
	}
	if _, err := tokenpack.Update(p.ID, 10_000, true); err != nil {
		t.Fatal(err)
	}
	if w := packRequest(h, p.APIKey, "/v1/chat/completions", strings.ReplaceAll(body, "plan/m1", "plan/other")); w.Code != 403 {
		t.Fatalf("model: %d %s", w.Code, w.Body)
	}
	db, err := sql.Open("sqlite", tokenpack.Path())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE token_packs SET expires=? WHERE id=?", time.Now().Add(-time.Hour).UnixMilli(), p.ID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if w := packRequest(h, p.APIKey, "/v1/chat/completions", body); w.Code != 410 {
		t.Fatalf("expired: %d %s", w.Code, w.Body)
	}
	if err := os.Remove(tokenpack.Path()); err != nil {
		t.Fatal(err)
	}
	if w := packRequest(h, p.APIKey, "/v1/chat/completions", body); w.Code != 503 {
		t.Fatalf("missing ledger: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("guarded request reached provider %d times", calls.Load())
	}
}

func TestTokenPackMissingAndInterruptedStreamKeepReservation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-usage", true: "stream-cut"}[stream], func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
			var calls atomic.Int64
			serveOn(t, "plan", "upstream-secret", []string{"m1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"cut\",\"choices\":[{\"delta\":{\"content\":\"some\"}}]}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			p, err := tokenpack.Create("Unknown", []string{"plan/m1"}, 10_000, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			body := `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			h := New().Front()
			packRequest(h, p.APIKey, "/v1/chat/completions", body)
			w := packRequest(h, p.APIKey, "/v1/chat/completions", body)
			if w.Code != 423 || calls.Load() != 1 {
				t.Fatalf("unknown usage admitted another call: %d, calls=%d, %s", w.Code, calls.Load(), w.Body)
			}
			p, err = tokenpack.Get(p.ID)
			if err != nil || p.ReservedTokens != 10_000 {
				t.Fatalf("reservation: %+v, %v", p, err)
			}
			logs, err := tokenpack.Logs(p.ID, 10)
			unknown := false
			for _, e := range logs {
				if e.Outcome == "uncertain" {
					unknown = true
				}
			}
			if err != nil || !unknown {
				t.Fatalf("unknown usage log: %+v %v", logs, err)
			}
		})
	}
}

func TestTokenPackStreamSettlesCompleteUsageIncludingCache(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	serveOn(t, "plan", "upstream-secret", []string{"m1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"full\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"full\",\"choices\":[],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":50,\"prompt_tokens_details\":{\"cached_tokens\":200}}}\n\ndata: [DONE]\n\n")
	}))
	p, err := tokenpack.Create("Stream", []string{"plan/m1"}, 10_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	w := packRequest(New().Front(), p.APIKey, "/v1/chat/completions", `{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("stream: %d %s", w.Code, w.Body)
	}
	p, err = tokenpack.Get(p.ID)
	if err != nil || p.UsedTokens != 350 || p.ReservedTokens != 0 {
		t.Fatalf("stream balance: %+v %v", p, err)
	}
}

func TestTokenPackConcurrentGatewayRequestsAreHeld(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	serveOn(t, "plan", "upstream-secret", []string{"m1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(400)
			return
		}
		close(started)
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":300,"completion_tokens":50}}`)
	}))
	p, err := tokenpack.Create("Concurrent", []string{"plan/m1"}, 10_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	h := New().Front()
	body := `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`
	done := make(chan *httptest.ResponseRecorder, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { done <- packRequest(h, p.APIKey, "/v1/chat/completions", body) }()
	select {
	case <-started:
	case <-ctx.Done():
		close(finish)
		t.Fatal("upstream was not reached")
	}
	w := packRequest(h, p.APIKey, "/v1/chat/completions", body)
	close(finish)
	first := <-done
	if w.Code != 409 || first.Code != 200 {
		t.Fatalf("concurrency: first=%d second=%d %s", first.Code, w.Code, w.Body)
	}
}

func TestTokenPackBoundsDecodedBodyBeforeReservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		compressed  bool
		serverLimit int64
		size        int
	}{
		{"plain", false, 0, 2 << 20}, {"gzip", true, 0, 2 << 20}, {"zstd", false, 0, 2 << 20}, {"server-limit", false, 1024, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
			calls := limitedUpstream(t, 0)
			p, err := tokenpack.Create("Body bounds", []string{"plan/m1"}, 10_000_000, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"model":"plan/m1","messages":[{"role":"user","content":"` + strings.Repeat("x", tc.size) + `"}]}`)
			if tc.compressed {
				var compressed bytes.Buffer
				z := gzip.NewWriter(&compressed)
				z.Write(body)
				z.Close()
				body = compressed.Bytes()
			}
			if tc.name == "zstd" {
				encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
				body = encoder.EncodeAll(body, nil)
				encoder.Close()
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+p.APIKey)
			if tc.compressed {
				r.Header.Set("Content-Encoding", "gzip")
			}
			if tc.name == "zstd" {
				r.Header.Set("Content-Encoding", "zstd")
			}
			s := New()
			s.requestLimits.body = tc.serverLimit
			w := httptest.NewRecorder()
			s.Front().ServeHTTP(w, r)
			if w.Code != 413 || calls.Load() != 0 {
				t.Fatalf("large body reached provider: status=%d calls=%d", w.Code, calls.Load())
			}
			p, err = tokenpack.Get(p.ID)
			if err != nil || p.UsedTokens != 0 || p.ReservedTokens != 0 {
				t.Fatalf("body rejection reserved allowance: %+v %v", p, err)
			}
			entries, err := tokenpack.Logs(p.ID, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.ReservedTokens != 0 || entry.TotalTokens != 0 {
					t.Fatalf("body rejection created reservation: %+v", entry)
				}
			}
		})
	}
}

type packSlowBody struct {
	io.Reader
	started, resume chan struct{}
	once            sync.Once
}

func (b *packSlowBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started); <-b.resume })
	return b.Reader.Read(p)
}
func (b *packSlowBody) Close() error { return nil }

func TestTokenPackSerializesBeforeReadingRequestBody(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	calls := limitedUpstream(t, 0)
	p, err := tokenpack.Create("Body serial", []string{"plan/m1"}, 10_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`
	slow := &packSlowBody{Reader: strings.NewReader(body), started: make(chan struct{}), resume: make(chan struct{})}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", slow)
	r.Header.Set("Authorization", "Bearer "+p.APIKey)
	h := New().Front()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, r); done <- w }()
	select {
	case <-slow.started:
	case <-time.After(10 * time.Second):
		close(slow.resume)
		t.Fatal("first body did not start reading")
	}
	w := packRequest(h, p.APIKey, "/v1/chat/completions", body)
	close(slow.resume)
	first := <-done
	if w.Code != 409 || first.Code != 200 || calls.Load() != 1 {
		t.Fatalf("concurrent bodies: first=%d second=%d calls=%d", first.Code, w.Code, calls.Load())
	}
}

func TestTokenPackPartialNonzeroUsageStaysUncertain(t *testing.T) {
	for _, counts := range []string{`"prompt_tokens":300`, `"completion_tokens":50`} {
		t.Run(counts, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
			serveOn(t, "plan", "upstream-secret", []string{"m1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{`+counts+`}}`)
			}))
			p, err := tokenpack.Create("Partial usage", []string{"plan/m1"}, 10_000, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			packRequest(New().Front(), p.APIKey, "/v1/chat/completions", `{"model":"plan/m1","messages":[{"role":"user","content":"hi"}]}`)
			p, err = tokenpack.Get(p.ID)
			if err != nil || p.ReservedTokens != 10_000 || p.UsedTokens != 0 {
				t.Fatalf("partial usage released allowance: %+v %v", p, err)
			}
		})
	}
}

func TestTokenPackUnknownCacheCountWithOutputStaysUncertain(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"unknown_cache","type":"message","role":"assistant","model":"m1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":300,"output_tokens":50,"magpie_cache_unknown":true}}`)
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "upstream-secret", Models: []string{"m1"}, Anthropic: up.URL}); err != nil {
		t.Fatal(err)
	}
	p, err := tokenpack.Create("Unknown cache", []string{"plan/m1"}, 10_000, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	packRequest(New().Front(), p.APIKey, "/v1/messages", `{"model":"plan/m1","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	p, err = tokenpack.Get(p.ID)
	if err != nil || p.ReservedTokens != 10_000 || p.UsedTokens != 0 {
		t.Fatalf("unknown cache released allowance: %+v %v", p, err)
	}
}

func TestTokenPackRejectsOverflowingUsageComponents(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if int64(maxInt) <= tokenpack.MaxTokens {
		t.Skip("overflow fixture requires a 64-bit int")
	}
	u := &packUsage{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), tokenPackContext{}, u))
	recordTokenPackUsage(r, usage.Record{Input: maxInt, Output: maxInt, CacheRead: 3, Status: 200})
	if !u.unknown || u.tokens != 0 {
		t.Fatalf("overflowing usage treated as certain: %+v", u)
	}
}

func TestTokenPackTextOnlyRejectsPaidMedia(t *testing.T) {
	for _, tc := range []struct {
		body    string
		blocked bool
	}{
		{`{"tools":[{"type":"image_generation"}]}`, true},
		{`{"content":[{"type":"document","source":{"type":"base64","data":"abc"}}]}`, true},
		{`{"input":[{"type":"input_video","video_url":"https://example.invalid/video"}]}`, true},
		{`{"modalities":["text","audio"]}`, true},
		{`{"modalities":["unknown"]}`, true},
		{`{"contents":[{"parts":[{"inline_data":{"mime_type":"audio/wav","data":"abc"}}]}]}`, true},
		{`{"tools":[{"type":"function","function":{"name":"image_generation","parameters":{"type":"object"}}}],"modalities":["text"]}`, false},
		{`{"tools":[{"type":"custom","name":"caption"}]}`, false},
		{`{"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object","properties":{"modalities":{"type":"array","items":{"type":"string"}}}}}}]}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var body any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatal(err)
			}
			if got := packMultimodal(body); got != tc.blocked {
				t.Fatalf("paid media blocked=%v, want %v", got, tc.blocked)
			}
		})
	}
}
