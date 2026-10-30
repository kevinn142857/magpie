package gui

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/yetone/magpie/internal/provider"
)

type quotaSearchDTO struct {
	Vendor            string `json:"vendor"`
	Name              string `json:"name"`
	BaseURL           string `json:"baseURL"`
	Enabled           bool   `json:"enabled"`
	KeyConfigured     bool   `json:"keyConfigured"`
	KeyMasked         string `json:"keyMasked"`
	Editable          bool   `json:"editable"`
	UnsupportedReason string `json:"unsupportedReason,omitempty"`
}

type quotaSearchVendorDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"baseURL"`
	KeyOptional bool   `json:"keyOptional"`
}

type quotaSearchForm struct {
	Vendor  *string `json:"vendor,omitempty"`
	BaseURL *string `json:"baseURL,omitempty"`
	APIKey  *string `json:"apiKey,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

// quotaSearchRoutes manages saved search services without issuing network calls.
// The enclosing quota handler supplies administrator and same-origin protection.
func quotaSearchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/quota-searches", func(w http.ResponseWriter, r *http.Request) {
		quotaProviderMu.Lock()
		searches, err := provider.StoredSearchAPIsChecked()
		quotaProviderMu.Unlock()
		if err != nil {
			quotaSearchError(w, err)
			return
		}
		out := make([]quotaSearchDTO, 0, len(searches))
		counts := make(map[string]int, len(searches))
		for _, search := range searches {
			counts[search.Vendor]++
		}
		for _, search := range searches {
			summary := quotaSearchSummary(search)
			if counts[search.Vendor] > 1 {
				summary.Editable = false
				summary.UnsupportedReason = "This search vendor has multiple saved entries and is read-only here."
			}
			out = append(out, summary)
		}
		vendors := make([]quotaSearchVendorDTO, 0, len(provider.SearchVendors))
		for _, vendor := range provider.SearchVendors {
			vendors = append(vendors, quotaSearchVendorDTO{vendor.ID, vendor.Name, vendor.Base, vendor.KeyOptional})
		}
		writeJSON(w, map[string]any{"searches": out, "vendors": vendors})
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		var form quotaSearchForm
		if !packDecode(w, r, &form) {
			return
		}
		creating := r.Method == http.MethodPost
		search, err := quotaSearchSave(r.PathValue("vendor"), form, creating)
		if err != nil {
			quotaSearchError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if creating {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusCreated)
		}
		writeJSON(w, map[string]any{"search": quotaSearchSummary(search)})
	}
	mux.HandleFunc("POST /api/quota-searches", save)
	mux.HandleFunc("PATCH /api/quota-searches/{vendor}", save)
}

func quotaSearchError(w http.ResponseWriter, err error) {
	var validation *quotaProviderError
	if errors.As(err, &validation) {
		packJSONError(w, validation.status, validation.message)
		return
	}
	packJSONError(w, http.StatusServiceUnavailable, "Search configuration is unavailable.")
}

func quotaSearchURL(raw string) (string, error) {
	invalid := func() (string, error) {
		return "", quotaProviderFail(400, "Enter an HTTP or HTTPS base URL without credentials, query parameters or fragments.")
	}
	if len(raw) > 2048 || strings.ContainsFunc(raw, unicode.IsControl) || strings.Contains(raw, "#") {
		return invalid()
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ContainsFunc(u.Path, unicode.IsControl) {
		return invalid()
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return invalid()
	}
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/"), nil
}

func quotaSearchSingleKey(key string) bool {
	return len(key) <= 8192 && !strings.ContainsAny(key, ",;") && !strings.ContainsFunc(key, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	})
}

func quotaSearchEditable(search provider.SearchAPI) bool {
	_, known := provider.SearchVendorOf(search.Vendor)
	_, err := quotaSearchURL(search.Base())
	return known && err == nil && search.Pick == provider.SearchPickTurns && len(search.Keys()) <= 1 && quotaSearchSingleKey(search.Key)
}

func quotaSearchSummary(search provider.SearchAPI) quotaSearchDTO {
	redact := func(text string) string {
		for _, key := range search.Keys() {
			text = strings.ReplaceAll(text, key, "[redacted]")
		}
		return text
	}
	base := ""
	if u, err := url.Parse(search.Base()); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
		// Redact decoded paths too: a legacy URL may percent-encode its key.
		u.Host, u.Path, u.RawPath = redact(u.Host), redact(u.Path), ""
		base = u.String()
	}
	out := quotaSearchDTO{
		Vendor: redact(search.Vendor), Name: redact(search.Name()), BaseURL: redact(base),
		Enabled: !search.Off, KeyConfigured: search.Key != "", KeyMasked: search.MaskedKey(), Editable: quotaSearchEditable(search),
	}
	if !out.Editable {
		out.UnsupportedReason = "Advanced search settings require the full configuration tools."
	}
	return out
}

// Search services and model suppliers share providers.json and therefore share
// the same read/merge/write lock. Partial edits always merge with the latest row.
func quotaSearchSave(vendor string, form quotaSearchForm, creating bool) (provider.SearchAPI, error) {
	quotaProviderMu.Lock()
	defer quotaProviderMu.Unlock()
	searches, err := provider.StoredSearchAPIsChecked()
	if err != nil {
		return provider.SearchAPI{}, err
	}
	if creating {
		if form.Vendor == nil {
			return provider.SearchAPI{}, quotaProviderFail(400, "Choose a search vendor.")
		}
		vendor = strings.ToLower(strings.TrimSpace(*form.Vendor))
	} else if form.Vendor != nil && strings.ToLower(strings.TrimSpace(*form.Vendor)) != vendor {
		return provider.SearchAPI{}, quotaProviderFail(400, "Search vendor cannot be changed.")
	}
	v, known := provider.SearchVendorOf(vendor)
	if !known {
		return provider.SearchAPI{}, quotaProviderFail(400, "Choose a supported search vendor.")
	}
	i := slices.IndexFunc(searches, func(search provider.SearchAPI) bool { return search.Vendor == vendor })
	if creating && i >= 0 {
		return provider.SearchAPI{}, quotaProviderFail(409, "This search vendor is already configured.")
	}
	if !creating && i < 0 {
		return provider.SearchAPI{}, quotaProviderFail(404, "Search service not found.")
	}
	if i >= 0 && slices.ContainsFunc(searches[i+1:], func(search provider.SearchAPI) bool { return search.Vendor == vendor }) {
		return provider.SearchAPI{}, quotaProviderFail(409, "This search vendor has multiple saved entries and is read-only here.")
	}
	search := provider.SearchAPI{Vendor: vendor, URL: v.Base}
	if i >= 0 {
		search = searches[i]
		if !quotaSearchEditable(search) {
			return search, quotaProviderFail(409, "This search service has advanced settings and is read-only here.")
		}
	}
	if !creating && form.BaseURL == nil && form.APIKey == nil && form.Enabled == nil {
		return search, quotaProviderFail(400, "Supply at least one search setting to update.")
	}
	previousBase, _ := quotaSearchURL(search.Base())
	if form.BaseURL != nil {
		if strings.ContainsFunc(*form.BaseURL, unicode.IsControl) {
			return search, quotaProviderFail(400, "The base URL must not contain control characters.")
		}
		search.URL = *form.BaseURL
		if strings.TrimSpace(search.URL) == "" {
			search.URL = v.Base
		}
	}
	base, err := quotaSearchURL(search.Base())
	if err != nil {
		return search, err
	}
	search.URL = base
	if v.Base != "" && base == strings.TrimRight(v.Base, "/") {
		// Keep vendor defaults implicit: keyless You.com selects its free
		// endpoint only when no custom URL is stored.
		search.URL = ""
	}
	newKey := ""
	if form.APIKey != nil {
		newKey = *form.APIKey
		if !quotaSearchSingleKey(newKey) {
			return search, quotaProviderFail(400, "Enter one API key without separators or whitespace.")
		}
	}
	if !creating && previousBase != base && newKey == "" && (search.Key != "" || vendor != "searxng") {
		return search, quotaProviderFail(400, "Changing the base URL requires an explicit new API key.")
	}
	if newKey != "" {
		search.Key = newKey
	}
	if search.Key == "" && !v.KeyOptional {
		return search, quotaProviderFail(400, "This search vendor requires an API key.")
	}
	if form.Enabled != nil {
		search.Off = !*form.Enabled
	}
	if err := provider.SaveSearchAPI(search); err != nil {
		return search, err
	}
	return search, nil
}
