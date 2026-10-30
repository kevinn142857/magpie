package gui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

// Serialize the complete read/merge/write operation. Network requests never hold
// this lock, and a partial edit always starts from the latest stored provider.
var quotaProviderMu sync.Mutex

type quotaProviderDTO struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Protocol          string   `json:"protocol"`
	BaseURL           string   `json:"baseURL"`
	Models            []string `json:"models"`
	Enabled           bool     `json:"enabled"`
	KeyConfigured     bool     `json:"keyConfigured"`
	KeyMasked         string   `json:"keyMasked"`
	Editable          bool     `json:"editable"`
	UnsupportedReason string   `json:"unsupportedReason,omitempty"`
}

type quotaProviderForm struct {
	ID       string    `json:"id,omitempty"`
	Name     *string   `json:"name,omitempty"`
	Protocol *string   `json:"protocol,omitempty"`
	BaseURL  *string   `json:"baseURL,omitempty"`
	APIKey   *string   `json:"apiKey,omitempty"`
	Models   *[]string `json:"models,omitempty"`
	Enabled  *bool     `json:"enabled,omitempty"`
}

type quotaProviderError struct {
	status  int
	message string
}

func (e *quotaProviderError) Error() string { return e.message }

func quotaProviderFail(status int, message string) error {
	return &quotaProviderError{status: status, message: message}
}

func quotaProviderErrorJSON(w http.ResponseWriter, err error) {
	var e *quotaProviderError
	if errors.As(err, &e) {
		packJSONError(w, e.status, e.message)
		return
	}
	packJSONError(w, http.StatusServiceUnavailable, "Provider configuration is unavailable.")
}

// quotaProviderRoutes exposes only API-key provider configuration to the quota
// console. The caller supplies the administrator and same-origin boundaries.
func quotaProviderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/quota-providers", func(w http.ResponseWriter, r *http.Request) {
		quotaProviderMu.Lock()
		ps, err := provider.Stored()
		quotaProviderMu.Unlock()
		if err != nil {
			quotaProviderErrorJSON(w, err)
			return
		}
		out := []quotaProviderDTO{}
		for _, p := range ps {
			if _, base := quotaProviderEndpoint(p); base != "" {
				out = append(out, quotaProviderSummary(p))
			}
		}
		writeJSON(w, map[string]any{"providers": out})
	})
	mux.HandleFunc("POST /api/quota-providers", func(w http.ResponseWriter, r *http.Request) {
		var f quotaProviderForm
		if !packDecode(w, r, &f) {
			return
		}
		p, err := quotaProviderSave("", f)
		if err != nil {
			quotaProviderErrorJSON(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"provider": quotaProviderSummary(p)})
	})
	mux.HandleFunc("PATCH /api/quota-providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		var f quotaProviderForm
		if !packDecode(w, r, &f) {
			return
		}
		p, err := quotaProviderSave(r.PathValue("id"), f)
		if err != nil {
			quotaProviderErrorJSON(w, err)
			return
		}
		writeJSON(w, map[string]any{"provider": quotaProviderSummary(p)})
	})
	mux.HandleFunc("POST /api/quota-providers/models", quotaProviderModels)
	mux.HandleFunc("POST /api/quota-providers/probe", quotaProviderProbe)
}

func quotaProviderEndpoint(p provider.Provider) (provider.Protocol, string) {
	for _, ep := range []struct {
		protocol provider.Protocol
		base     string
	}{{provider.Chat, p.Chat}, {provider.Responses, p.Responses}, {provider.Anthropic, p.Anthropic}, {provider.Gemini, p.Gemini}} {
		if ep.base != "" {
			return ep.protocol, ep.base
		}
	}
	return "", ""
}

// Unknown provider settings stay read-only: reconstructing their authentication
// from the small form could discard settings or carry credentials to a new URL.
func quotaProviderEditable(p provider.Provider) bool {
	if p.Account != nil {
		return false
	}
	b, err := json.Marshal(p)
	if err != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return false
	}
	endpoints := 0
	for field := range fields {
		switch field {
		case "id", "name", "key", "baseAPI", "models", "noModels", "off":
		case "chat", "responses", "anthropic", "gemini":
			endpoints++
		default:
			return false
		}
	}
	proto, base := quotaProviderEndpoint(p)
	_, err = quotaProviderURL(base, proto)
	return endpoints == 1 && err == nil
}

func quotaProviderRedact(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	return s
}

func quotaProviderSummary(p provider.Provider) quotaProviderDTO {
	proto, base := quotaProviderEndpoint(p)
	// Imported legacy URLs may contain credentials. They are never editable or
	// sent by this API, and their URL credentials are absent from its summary.
	if u, err := url.Parse(base); err == nil {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		base = u.String()
	} else {
		base = ""
	}
	models := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		models = append(models, quotaProviderRedact(m, p.Key))
	}
	out := quotaProviderDTO{
		ID: p.ID, Name: quotaProviderRedact(p.Name, p.Key), Protocol: string(proto),
		BaseURL: quotaProviderRedact(base, p.Key), Models: models, Enabled: !p.Off,
		KeyConfigured: p.Key != "" || len(p.Keys) != 0 || len(p.Headers) != 0,
		KeyMasked:     provider.Mask(p.Key), Editable: quotaProviderEditable(p),
	}
	if !out.Editable {
		out.UnsupportedReason = "Advanced provider settings require the full configuration tools."
	}
	return out
}

func quotaProviderURL(raw string, proto provider.Protocol) (string, error) {
	if proto != provider.Chat && proto != provider.Responses && proto != provider.Anthropic && proto != provider.Gemini {
		return "", quotaProviderFail(400, "Choose a supported provider protocol.")
	}
	raw = strings.TrimSpace(raw)
	if len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t") {
		return "", quotaProviderFail(400, "Enter a valid HTTP or HTTPS base URL without credentials or query parameters.")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", quotaProviderFail(400, "Enter a valid HTTP or HTTPS base URL without credentials or query parameters.")
	}
	u.Host = strings.ToLower(u.Host)
	return provider.DetectBase(u.String(), proto), nil
}

func quotaProviderLocal(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func quotaProviderModelsValid(models []string) ([]string, error) {
	if len(models) > 500 {
		return nil, quotaProviderFail(400, "Choose at most 500 models.")
	}
	out := []string{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 300 || strings.ContainsFunc(model, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
			return nil, quotaProviderFail(400, "Enter valid model identifiers.")
		}
		if !slices.Contains(out, model) {
			out = append(out, model)
		}
	}
	return out, nil
}

func quotaProviderLookup(ps []provider.Provider, id string) (provider.Provider, error) {
	for _, p := range ps {
		if p.ID == id {
			return p, nil
		}
	}
	return provider.Provider{}, quotaProviderFail(404, "Provider not found.")
}

// The draft holds only the explicitly supported fields. A saved credential is
// reused solely for the same normalized protocol and complete base URL.
func quotaProviderDraft(old *provider.Provider, f quotaProviderForm) (provider.Provider, error) {
	p := provider.Provider{}
	var proto provider.Protocol
	var base string
	if old != nil {
		if !quotaProviderEditable(*old) {
			return p, quotaProviderFail(409, "This provider has advanced settings and is read-only here.")
		}
		p.ID, p.Name, p.Key, p.Models, p.Off = old.ID, old.Name, old.Key, slices.Clone(old.Models), old.Off
		proto, base = quotaProviderEndpoint(*old)
	}
	if f.Name != nil {
		p.Name = strings.TrimSpace(*f.Name)
	}
	if p.Name == "" && old == nil {
		p.Name = "Provider" // Unstored connectivity forms need no display name.
	}
	if utf8.RuneCountInString(p.Name) > 120 || p.Name == "" || strings.ContainsFunc(p.Name, unicode.IsControl) {
		return p, quotaProviderFail(400, "Enter a provider name of at most 120 characters.")
	}
	if f.Protocol != nil {
		proto = provider.Protocol(*f.Protocol)
	}
	if f.BaseURL != nil {
		base = *f.BaseURL
	}
	var err error
	base, err = quotaProviderURL(base, proto)
	if err != nil {
		return p, err
	}
	if old != nil && old.Key != "" && (f.APIKey == nil || strings.TrimSpace(*f.APIKey) == "") {
		oldProto, oldBase := quotaProviderEndpoint(*old)
		oldBase, err = quotaProviderURL(oldBase, oldProto)
		if err != nil || oldProto != proto || oldBase != base {
			return p, quotaProviderFail(400, "Enter a new API key when changing the base URL or protocol.")
		}
	}
	if f.APIKey != nil && strings.TrimSpace(*f.APIKey) != "" {
		p.Key = strings.TrimSpace(*f.APIKey)
	}
	if len(p.Key) > 4096 || strings.ContainsFunc(p.Key, func(r rune) bool { return r < 33 || r > 126 }) {
		return p, quotaProviderFail(400, "Enter a valid API key.")
	}
	if p.Key == "" && !quotaProviderLocal(base) {
		return p, quotaProviderFail(400, "An API key is required for a public provider.")
	}
	if f.Models != nil {
		p.Models = *f.Models
	}
	p.Models, err = quotaProviderModelsValid(p.Models)
	if err != nil {
		return p, err
	}
	p.NoModels = len(p.Models) == 0
	if f.Enabled != nil {
		p.Off = !*f.Enabled
	}
	p.BaseAPI = string(proto)
	switch proto {
	case provider.Chat:
		p.Chat = base
	case provider.Responses:
		p.Responses = base
	case provider.Anthropic:
		p.Anthropic = base
	case provider.Gemini:
		p.Gemini = base
	}
	return p, nil
}

func quotaProviderSave(id string, f quotaProviderForm) (provider.Provider, error) {
	quotaProviderMu.Lock()
	defer quotaProviderMu.Unlock()
	ps, err := provider.Stored()
	if err != nil {
		return provider.Provider{}, err
	}
	if f.ID != "" && f.ID != id {
		return provider.Provider{}, quotaProviderFail(400, "Provider ID cannot be changed.")
	}
	var old *provider.Provider
	if id != "" {
		p, err := quotaProviderLookup(ps, id)
		if err != nil {
			return p, err
		}
		old = &p
	} else if f.Name == nil || strings.TrimSpace(*f.Name) == "" {
		return provider.Provider{}, quotaProviderFail(400, "Enter a provider name.")
	}
	p, err := quotaProviderDraft(old, f)
	if err != nil {
		return p, err
	}
	if old == nil {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return p, err
		}
		p.ID = "upstream-" + hex.EncodeToString(b[:])
	}
	added, replaced, err := provider.Restore([]provider.Provider{p})
	if err != nil {
		return p, err
	}
	if (old == nil && (added != 1 || replaced != 0)) || (old != nil && (added != 0 || replaced != 1)) {
		return p, errors.New("provider was not stored")
	}
	return p, nil
}

func quotaProviderReadDraft(w http.ResponseWriter, r *http.Request) (provider.Provider, bool) {
	var f quotaProviderForm
	if !packDecode(w, r, &f) {
		return provider.Provider{}, false
	}
	quotaProviderMu.Lock()
	var old *provider.Provider
	var err error
	if f.ID != "" {
		var ps []provider.Provider
		ps, err = provider.Stored()
		if err == nil {
			var p provider.Provider
			p, err = quotaProviderLookup(ps, f.ID)
			old = &p
		}
	}
	var p provider.Provider
	if err == nil {
		p, err = quotaProviderDraft(old, f)
	}
	quotaProviderMu.Unlock()
	if err != nil {
		quotaProviderErrorJSON(w, err)
		return p, false
	}
	// The ephemeral draft has no saved-provider identity, account, secondary
	// keys or headers; provider-specific discovery cannot reuse other secrets.
	p.ID = ""
	return p, true
}

// Quota-console requests never follow a redirect, and cannot read unbounded
// upstream replies. The default transport retains the application's proxy and
// test network policies without changing the process-wide default client.
type quotaProviderTransport struct {
	base   http.RoundTripper
	origin string
}

func (t quotaProviderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != t.origin {
		return nil, errors.New("provider request changed origin")
	}
	res, err := t.base.RoundTrip(r)
	if err == nil && res.Body != nil {
		res.Body = &quotaProviderBody{Reader: io.LimitReader(res.Body, 1<<20), Closer: res.Body}
	}
	return res, err
}

type quotaProviderBody struct {
	io.Reader
	io.Closer
}

func quotaProviderContext(r *http.Request, p provider.Provider) (context.Context, context.CancelFunc) {
	_, base := quotaProviderEndpoint(p)
	u, _ := url.Parse(base)
	client := &http.Client{
		Timeout:       15 * time.Second,
		Transport:     quotaProviderTransport{base: http.DefaultTransport, origin: u.Scheme + "://" + u.Host},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	return catalog.WithHTTPClient(ctx, client), cancel
}

func quotaProviderModels(w http.ResponseWriter, r *http.Request) {
	p, ok := quotaProviderReadDraft(w, r)
	if !ok {
		return
	}
	ctx, cancel := quotaProviderContext(r, p)
	defer cancel()
	models, err := p.List(ctx)
	if err != nil {
		packJSONError(w, http.StatusBadGateway, "The provider model list could not be read. Check the endpoint and credentials.")
		return
	}
	ids := []string{}
	for _, model := range models {
		// An upstream can echo a credential as a model name. Never return it.
		if p.Key != "" && strings.Contains(model.ID, p.Key) {
			continue
		}
		if _, err := quotaProviderModelsValid([]string{model.ID}); err == nil && !slices.Contains(ids, model.ID) {
			ids = append(ids, model.ID)
		}
		if len(ids) == 500 {
			break
		}
	}
	writeJSON(w, map[string]any{"models": ids, "count": len(ids)})
}

func quotaProviderProbe(w http.ResponseWriter, r *http.Request) {
	p, ok := quotaProviderReadDraft(w, r)
	if !ok {
		return
	}
	if len(p.Models) == 0 {
		packJSONError(w, http.StatusBadRequest, "Choose a model to probe.")
		return
	}
	ctx, cancel := quotaProviderContext(r, p)
	defer cancel()
	proto, _ := quotaProviderEndpoint(p)
	result := p.TestEndpoint(ctx, proto, p.Models[0])
	message := "The provider test failed. Check the endpoint and credentials."
	if result.OK {
		message = "The provider accepted the test request."
	}
	writeJSON(w, map[string]any{"ok": result.OK, "statusCode": result.Status, "model": quotaProviderRedact(p.Models[0], p.Key), "protocol": proto, "millis": result.Millis, "message": message})
}
