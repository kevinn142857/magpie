package gui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/tokenpack"
)

func quotaPanelEnabled() bool { return os.Getenv("MAGPIE_QUOTA_PANEL") == "1" }

// tokenPackRow deliberately excludes API keys and raw share credentials from
// listings. The administrator can copy the package's independent share URL.
type tokenPackRow struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Models          []string   `json:"models"`
	TotalTokens     int64      `json:"totalTokens"`
	UsedTokens      int64      `json:"usedTokens"`
	ReservedTokens  int64      `json:"reservedTokens"`
	RemainingTokens int64      `json:"remainingTokens"`
	Enabled         bool       `json:"enabled"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	ShareURL        string     `json:"shareURL,omitempty"`
}

func packRow(p tokenpack.Pack) tokenPackRow {
	row := tokenPackRow{ID: p.ID, Name: p.Name, Models: p.Models, TotalTokens: p.TotalTokens,
		UsedTokens: p.UsedTokens, ReservedTokens: p.ReservedTokens, RemainingTokens: p.RemainingTokens,
		Enabled: p.Enabled, CreatedAt: p.CreatedAt}
	if !p.ExpiresAt.IsZero() {
		row.ExpiresAt = &p.ExpiresAt
	}
	if base := gateway.PublicURL(); base != "" {
		row.ShareURL = strings.TrimRight(base, "/") + "/share/" + url.PathEscape(p.ShareToken)
	}
	return row
}

// quotaPanelHandler exposes only package operations and its small static UI.
// webGuard remains outside it; no other desktop management API is mounted.
func quotaPanelHandler() http.Handler {
	mux := http.NewServeMux()
	for _, item := range []struct{ route, asset, contentType string }{
		{"GET /{$}", "quota/index.html", "text/html; charset=utf-8"},
		{"GET /quota/app.js", "quota/app.js", "text/javascript; charset=utf-8"},
		{"GET /quota/i18n.js", "quota/i18n.js", "text/javascript; charset=utf-8"},
		{"GET /quota/app.css", "quota/app.css", "text/css; charset=utf-8"},
	} {
		mux.HandleFunc(item.route, func(w http.ResponseWriter, r *http.Request) {
			body, err := assets.ReadFile("assets/" + item.asset)
			if err != nil {
				http.Error(w, "quota console is unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", item.contentType)
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
		})
	}
	mux.HandleFunc("GET /api/token-packs", func(w http.ResponseWriter, r *http.Request) {
		packs, err := tokenpack.List()
		if err != nil {
			packError(w, err)
			return
		}
		rows := make([]tokenPackRow, 0, len(packs))
		for _, p := range packs {
			rows = append(rows, packRow(p))
		}
		writeJSON(w, struct {
			Packs   []tokenPackRow `json:"packs"`
			BaseURL string         `json:"baseURL"`
		}{rows, packBaseURL()})
	})
	mux.HandleFunc("POST /api/token-packs", func(w http.ResponseWriter, r *http.Request) {
		if gateway.PublicURL() == "" {
			packJSONError(w, http.StatusServiceUnavailable, "Configure MAGPIE_PUBLIC_URL before creating share links.")
			return
		}
		var in struct {
			Name        string    `json:"name"`
			Models      []string  `json:"models"`
			TotalTokens int64     `json:"totalTokens"`
			ExpiresAt   time.Time `json:"expiresAt"`
		}
		if !packDecode(w, r, &in) {
			return
		}
		p, err := tokenpack.Create(in.Name, in.Models, in.TotalTokens, in.ExpiresAt)
		if err != nil {
			packError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]tokenPackRow{"pack": packRow(p)})
	})
	mux.HandleFunc("PATCH /api/token-packs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TotalTokens *int64 `json:"totalTokens"`
			Enabled     *bool  `json:"enabled"`
		}
		if !packDecode(w, r, &in) {
			return
		}
		if in.TotalTokens == nil && in.Enabled == nil {
			packJSONError(w, http.StatusBadRequest, "Provide totalTokens or enabled.")
			return
		}
		p, err := tokenpack.Patch(r.PathValue("id"), in.TotalTokens, in.Enabled)
		if err != nil {
			packError(w, err)
			return
		}
		writeJSON(w, map[string]tokenPackRow{"pack": packRow(p)})
	})
	mux.HandleFunc("GET /api/token-packs/models", func(w http.ResponseWriter, r *http.Request) {
		type modelRow struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		models := []modelRow{}
		for _, e := range provider.Catalog() {
			id := e.Provider.ID + "/" + e.Model
			if e.Group != "" {
				id = provider.GroupPrefix + e.Group
			}
			models = append(models, modelRow{ID: id, Name: e.Name})
		}
		writeJSON(w, map[string][]modelRow{"models": models})
	})
	mux.HandleFunc("GET /api/token-packs/logs", packLogs)
	mux.HandleFunc("POST /api/token-packs/logs/{id}/settle", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ActualTokens *int64 `json:"actualTokens"`
		}
		if !packDecode(w, r, &in) {
			return
		}
		if in.ActualTokens == nil {
			packJSONError(w, http.StatusBadRequest, "Provide actualTokens, including zero when no usage occurred.")
			return
		}
		entry, err := tokenpack.SettleRequest(r.PathValue("id"), *in.ActualTokens)
		if err != nil {
			packError(w, err)
			return
		}
		writeJSON(w, map[string]tokenpack.Log{"log": entry})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host {
					packJSONError(w, http.StatusForbidden, "Cross-origin changes are refused.")
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func packBaseURL() string {
	if base := gateway.PublicURL(); base != "" {
		return strings.TrimRight(base, "/") + "/v1"
	}
	return ""
}

func packDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
		packJSONError(w, http.StatusBadRequest, "Use Content-Type: application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		packJSONError(w, http.StatusBadRequest, "Invalid request fields or JSON.")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		packJSONError(w, http.StatusBadRequest, "Send exactly one JSON object.")
		return false
	}
	return true
}

func packJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": message})
}

func packError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, tokenpack.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, tokenpack.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, tokenpack.ErrConflict), errors.Is(err, tokenpack.ErrBusy):
		status = http.StatusConflict
	}
	if status == http.StatusServiceUnavailable {
		log.Printf("token package operation failed: %v", err)
		packJSONError(w, status, "The package ledger is unavailable; no allowance was granted.")
		return
	}
	packJSONError(w, status, err.Error())
}

func packLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			packJSONError(w, http.StatusBadRequest, "Log limit must be between 1 and 500.")
			return
		}
		limit = n
	}
	entries, err := tokenpack.Logs(r.URL.Query().Get("pack"), limit)
	if err != nil {
		packError(w, err)
		return
	}
	packs, err := tokenpack.List()
	if err != nil {
		packError(w, err)
		return
	}
	names := make(map[string]string, len(packs))
	for _, p := range packs {
		names[p.ID] = p.Name
	}
	type row struct {
		ID         string    `json:"id"`
		PackID     string    `json:"packId"`
		PackName   string    `json:"packName"`
		Model      string    `json:"model"`
		Tokens     int64     `json:"tokens"`
		Status     string    `json:"status"`
		Outcome    string    `json:"outcome"`
		CreatedAt  time.Time `json:"createdAt"`
		DurationMS int64     `json:"durationMs"`
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		ms := int64(0)
		if !e.CompletedAt.IsZero() {
			ms = max(0, e.CompletedAt.Sub(e.CreatedAt).Milliseconds())
		}
		outcome := e.Outcome
		if outcome == "rejected" {
			outcome = "failed"
		}
		if outcome == "settled" {
			outcome = "completed"
			if e.StatusCode >= http.StatusBadRequest {
				outcome = "failed"
			}
		}
		status := outcome
		if e.StatusCode > 0 {
			status = strconv.Itoa(e.StatusCode)
		}
		rows = append(rows, row{e.ID, e.PackID, names[e.PackID], e.Model, e.TotalTokens, status, outcome, e.CreatedAt, ms})
	}
	writeJSON(w, map[string][]row{"logs": rows})
}

// tokenPackPublic bypasses only the administrator cookie, and only for exact
// read-only share paths. The gateway and administrator API retain their guards.
func tokenPackPublic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !quotaPanelEnabled() || !strings.HasPrefix(r.URL.Path, "/share/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		token := strings.TrimPrefix(r.URL.Path, "/share/")
		if len(token) < 32 || len(token) > 128 || strings.Trim(token, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		p, err := tokenpack.ByShare(token)
		if err != nil {
			packError(w, err)
			return
		}
		if !p.Enabled || (!p.ExpiresAt.IsZero() && !time.Now().Before(p.ExpiresAt)) {
			http.Error(w, "This package is disabled or expired.", http.StatusGone)
			return
		}
		if packBaseURL() == "" {
			http.Error(w, "The public API address is not configured.", http.StatusServiceUnavailable)
			return
		}
		var nonceBytes [16]byte
		if _, err := rand.Read(nonceBytes[:]); err != nil {
			http.Error(w, "The share page is unavailable.", http.StatusServiceUnavailable)
			return
		}
		nonce := hex.EncodeToString(nonceBytes[:])
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+"'; script-src 'nonce-"+nonce+"'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		var body bytes.Buffer
		expiry := ""
		if !p.ExpiresAt.IsZero() {
			expiry = p.ExpiresAt.In(time.FixedZone("UTC+08:00", 8*60*60)).Format("2006-01-02 15:04 UTC+08:00")
		}
		if err := tokenPackShareTemplate.Execute(&body, struct {
			Pack    tokenpack.Pack
			BaseURL string
			RootURL string
			Expiry  string
			Nonce   string
		}{p, packBaseURL(), strings.TrimRight(gateway.PublicURL(), "/"), expiry, nonce}); err != nil {
			http.Error(w, "The share page is unavailable.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(body.Bytes())
		}
	})
}

var tokenPackShareTemplate = template.Must(template.New("token-pack-share").Parse(tokenPackShareHTML))
