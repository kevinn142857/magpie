package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func quotaSearchRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestQuotaSearchEditsKeepPriority(t *testing.T) {
	quotaPanelHome(t)
	if err := provider.RestoreSearchAPIs([]provider.SearchAPI{
		{Vendor: "tavily", Key: "tvly-priority-fixture-key"},
		{Vendor: "brave", Key: "brave-priority-fixture-key"},
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	quotaSearchRoutes(mux)
	if w := quotaSearchRequest(mux, "PATCH", "/api/quota-searches/brave", `{"enabled":false}`); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	stored := provider.StoredSearchAPIs()
	if len(stored) != 2 || stored[0].Vendor != "tavily" || stored[1].Vendor != "brave" || !stored[1].Off {
		t.Fatal("editing a search service changed the vendor priority order")
	}
}

func TestQuotaSearchRejectsControlOnlyBaseURL(t *testing.T) {
	quotaPanelHome(t)
	mux := http.NewServeMux()
	quotaSearchRoutes(mux)
	for _, base := range []string{"\n", "\t", " \r "} {
		body, _ := json.Marshal(map[string]string{"vendor": "tavily", "apiKey": "tvly-fixture-key", "baseURL": base})
		if w := quotaSearchRequest(mux, "POST", "/api/quota-searches", string(body)); w.Code != 400 {
			t.Fatalf("control-only base URL accepted: status %d", w.Code)
		}
	}
}

func TestQuotaSearchYouFreeDefaultPreservesVendorRouting(t *testing.T) {
	quotaPanelHome(t)
	mux := http.NewServeMux()
	quotaSearchRoutes(mux)
	if w := quotaSearchRequest(mux, "POST", "/api/quota-searches", `{"vendor":"you"}`); w.Code != 201 {
		t.Fatalf("keyless You.com creation: %d %s", w.Code, w.Body.String())
	}
	stored := provider.StoredSearchAPIs()
	if len(stored) != 1 || stored[0].URL != "" || stored[0].Key != "" {
		t.Fatal("the default keyless You.com search no longer selects its existing free endpoint")
	}
}

func TestQuotaSearchDuplicateLegacyVendorIsReadOnly(t *testing.T) {
	quotaPanelHome(t)
	legacy := []provider.SearchAPI{
		{Vendor: "tavily", Key: "tvly-duplicate-first-fixture"},
		{Vendor: "tavily", Key: "tvly-duplicate-second-fixture"},
	}
	if err := provider.MirrorSearchAPIs(legacy); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	quotaSearchRoutes(mux)
	w := quotaSearchRequest(mux, "GET", "/api/quota-searches", "")
	var result struct {
		Searches []struct {
			Editable          bool
			UnsupportedReason string
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Searches) != 2 {
		t.Fatalf("duplicate vendor listing: %d", w.Code)
	}
	for _, row := range result.Searches {
		if row.Editable || row.UnsupportedReason == "" {
			t.Fatal("ambiguous duplicate vendor was editable")
		}
	}
	if w := quotaSearchRequest(mux, "PATCH", "/api/quota-searches/tavily", `{"enabled":false}`); w.Code != 409 {
		t.Fatalf("duplicate vendor edit: %d, want 409", w.Code)
	}
	stored := provider.StoredSearchAPIs()
	if len(stored) != 2 || stored[0] != legacy[0] || stored[1] != legacy[1] {
		t.Fatal("editing a duplicate vendor lost saved credentials")
	}
}

func TestQuotaSearchListOffersAllVendors(t *testing.T) {
	quotaPanelHome(t)
	w := quotaSearchRequest(Handler(webHost{}, nil), "GET", "/api/quota-searches", "")
	var got struct {
		Searches []any
		Vendors  []struct {
			ID, Name, BaseURL string
			KeyOptional       bool
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Searches == nil || len(got.Searches) != 0 || len(got.Vendors) != len(provider.SearchVendors) {
		t.Fatalf("empty search configuration: %d %s", w.Code, w.Body.String())
	}
	for i, v := range provider.SearchVendors {
		if actual := got.Vendors[i]; actual.ID != v.ID || actual.Name != v.Name || actual.BaseURL != v.Base || actual.KeyOptional != v.KeyOptional {
			t.Fatalf("vendor %q defaults changed", v.ID)
		}
	}
}

func TestQuotaSearchEditsRetainKeysOnlyAtTheSameCompleteURL(t *testing.T) {
	quotaPanelHome(t)
	h := Handler(webHost{}, nil)
	const oldKey, newKey = "tvly-original-fixture-secret", "tvly-replacement-fixture-secret"
	if w := quotaSearchRequest(h, "POST", "/api/quota-searches", `{"vendor":"tavily","baseURL":"https://SEARCH.example:8443/api/","apiKey":"`+oldKey+`"}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := quotaSearchRequest(h, "PATCH", "/api/quota-searches/tavily", `{"baseURL":"https://search.example:8443/api","apiKey":"","enabled":false}`); w.Code != 200 {
		t.Fatalf("same endpoint edit: %d %s", w.Code, w.Body.String())
	}
	for _, base := range []string{"https://search.example:8443/other", "https://search.example:8444/api", "http://search.example:8443/api", "https://other.example:8443/api"} {
		for _, keyField := range []string{"", `,"apiKey":""`} {
			body := `{"baseURL":"` + base + `"` + keyField + `}`
			if w := quotaSearchRequest(h, "PATCH", "/api/quota-searches/tavily", body); w.Code != 400 || strings.Contains(w.Body.String(), oldKey) {
				t.Fatalf("changed endpoint accepted a saved key: %d %s", w.Code, w.Body.String())
			}
		}
	}
	stored := provider.StoredSearchAPIs()
	if len(stored) != 1 || stored[0].Key != oldKey || stored[0].Base() != "https://search.example:8443/api" || !stored[0].Off {
		t.Fatal("rejected edits altered the search configuration")
	}
	if w := quotaSearchRequest(h, "PATCH", "/api/quota-searches/tavily", `{"baseURL":"https://other.example/new","apiKey":"`+newKey+`","enabled":true}`); w.Code != 200 || strings.Contains(w.Body.String(), newKey) {
		t.Fatalf("explicit replacement key: %d %s", w.Code, w.Body.String())
	}
	if stored := provider.StoredSearchAPIs(); len(stored) != 1 || stored[0].Key != newKey || stored[0].Base() != "https://other.example/new" || stored[0].Off {
		t.Fatal("replacement credential did not persist")
	}
	for _, item := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/quota-searches", `{"vendor":"tavily","apiKey":"duplicate"}`, 409},
		{"PATCH", "/api/quota-searches/tavily", `{"vendor":"brave","apiKey":"another-key"}`, 400},
		{"PATCH", "/api/quota-searches/brave", `{"enabled":false}`, 404},
	} {
		if w := quotaSearchRequest(h, item.method, item.path, item.body); w.Code != item.status {
			t.Fatalf("%s %s: %d, want %d", item.method, item.path, w.Code, item.status)
		}
	}
}

func TestQuotaSearchRejectsUnsafeAndUnknownInput(t *testing.T) {
	quotaPanelHome(t)
	h := Handler(webHost{}, nil)
	for _, body := range []string{
		`null`, `{}`, `{"vendor":"unknown","apiKey":"secret"}`, `{"vendor":"searxng"}`,
		`{"vendor":"tavily"}`, `{"vendor":"tavily","apiKey":"a,b"}`,
		`{"vendor":"tavily","apiKey":"a;b"}`, `{"vendor":"tavily","apiKey":"a b"}`,
		`{"vendor":"tavily","apiKey":"secret\n"}`, `{"vendor":"tavily","apiKey":"secret","unexpected":true}`,
		`{"vendor":"tavily","apiKey":"secret"} {}`,
		`{"vendor":"tavily","apiKey":"` + strings.Repeat("x", 65536) + `"}`,
	} {
		if w := quotaSearchRequest(h, "POST", "/api/quota-searches", body); w.Code != 400 {
			t.Errorf("invalid form accepted: status %d", w.Code)
		}
	}
	for _, base := range []string{"ftp://search.example", "https://u:p@search.example", "https://search.example/?q=secret", "https://search.example/?", "https://search.example/#secret", "https://search.example/#", "https://search.example/\n", "https://search.example/%0a", "https://search.example:invalid"} {
		body, _ := json.Marshal(map[string]any{"vendor": "searxng", "baseURL": base})
		if w := quotaSearchRequest(h, "POST", "/api/quota-searches", string(body)); w.Code != 400 {
			t.Errorf("unsafe URL accepted: status %d", w.Code)
		}
	}
	if stored := provider.StoredSearchAPIs(); len(stored) != 0 {
		t.Fatal("invalid forms saved configuration")
	}
}

func TestQuotaSearchLegacySecretsAreRedactedAndReadOnly(t *testing.T) {
	quotaPanelHome(t)
	const one, two, ordered = "tvly-first-fixture-private-key", "tvly-second-fixture-private-key", "brave-ordered-fixture-private-key"
	legacy := []provider.SearchAPI{
		{Vendor: "tavily", Key: one + "," + two, URL: "https://" + one + ":" + two + "@search.example/" + one + "?secret=" + two},
		{Vendor: "brave", Key: ordered, Pick: provider.SearchPickOrder},
	}
	if err := provider.RestoreSearchAPIs(legacy); err != nil {
		t.Fatal(err)
	}
	h := Handler(webHost{}, nil)
	w := quotaSearchRequest(h, "GET", "/api/quota-searches", "")
	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	for _, key := range []string{one, two, ordered} {
		if strings.Contains(w.Body.String(), key) {
			t.Fatal("search list disclosed a saved secret")
		}
	}
	var got struct {
		Searches []struct {
			Editable, KeyConfigured    bool
			UnsupportedReason, BaseURL string
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Searches) != 2 {
		t.Fatal("legacy searches missing")
	}
	for _, s := range got.Searches {
		if s.Editable || !s.KeyConfigured || s.UnsupportedReason == "" || strings.Contains(s.BaseURL, "@") || strings.Contains(s.BaseURL, "?") {
			t.Fatalf("legacy configuration was not safely summarized: %s", w.Body.String())
		}
	}
	for _, vendor := range []string{"tavily", "brave"} {
		if w := quotaSearchRequest(h, "PATCH", "/api/quota-searches/"+vendor, `{"enabled":false,"apiKey":"replacement"}`); w.Code != 409 {
			t.Fatalf("advanced search was editable: %d", w.Code)
		}
	}
	if actual := provider.StoredSearchAPIs(); len(actual) != len(legacy) || actual[0] != legacy[0] || actual[1] != legacy[1] {
		t.Fatal("read-only edits changed legacy configuration")
	}
}

func TestQuotaSearchPartialEditsSerializeTheMerge(t *testing.T) {
	quotaPanelHome(t)
	h := Handler(webHost{}, nil)
	if w := quotaSearchRequest(h, "POST", "/api/quota-searches", `{"vendor":"searxng","baseURL":"http://localhost:8888/search-base"}`); w.Code != 201 {
		t.Fatalf("create: %d", w.Code)
	}
	for iteration := 0; iteration < 12; iteration++ {
		if w := quotaSearchRequest(h, "PATCH", "/api/quota-searches/searxng", `{"enabled":true}`); w.Code != 200 {
			t.Fatalf("reset: %d", w.Code)
		}
		start := make(chan struct{})
		var group sync.WaitGroup
		statuses := make(chan int, 2)
		base := fmt.Sprintf("http://localhost:8888/revision-%d", iteration)
		for _, body := range []string{`{"enabled":false}`, `{"baseURL":"` + base + `"}`} {
			group.Add(1)
			go func(body string) {
				defer group.Done()
				<-start
				statuses <- quotaSearchRequest(h, "PATCH", "/api/quota-searches/searxng", body).Code
			}(body)
		}
		close(start)
		group.Wait()
		close(statuses)
		for status := range statuses {
			if status != 200 {
				t.Fatalf("concurrent patch: %d", status)
			}
		}
		stored := provider.StoredSearchAPIs()
		if len(stored) != 1 || !stored[0].Off || stored[0].Base() != base {
			t.Fatal("concurrent partial edit lost the other change")
		}
	}
}

func TestQuotaSearchUnreadableConfigurationFailsClosed(t *testing.T) {
	quotaPanelHome(t)
	path := provider.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const broken = `{"searches":[broken-secret`
	if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	h := Handler(webHost{}, nil)
	for _, item := range []struct{ method, body string }{{"GET", ""}, {"POST", `{"vendor":"searxng","baseURL":"http://localhost:8888"}`}} {
		w := quotaSearchRequest(h, item.method, "/api/quota-searches", item.body)
		if w.Code != 503 || strings.Contains(w.Body.String(), "broken-secret") {
			t.Fatalf("unreadable configuration response: %d %s", w.Code, w.Body.String())
		}
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != broken {
		t.Fatal("unreadable search configuration was overwritten")
	}
}

func TestQuotaSearchCreateKeepsKeyPrivate(t *testing.T) {
	quotaPanelHome(t)
	h := Handler(webHost{}, nil)
	const key = "tvly-private-fixture-0123456789abcdef"
	w := quotaSearchRequest(h, "POST", "/api/quota-searches", `{"vendor":"tavily","apiKey":"`+key+`","enabled":false}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("search creation: status %d, want 201; %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), key) {
		t.Fatal("search creation returned the saved secret")
	}
	var result struct {
		Search struct {
			Vendor, Name, BaseURL, KeyMasked string
			Enabled, KeyConfigured, Editable bool
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if s := result.Search; s.Vendor != "tavily" || s.Name != "Tavily" || s.BaseURL != "https://api.tavily.com" || s.Enabled || !s.KeyConfigured || !s.Editable || s.KeyMasked == "" {
		t.Fatalf("unexpected search summary: %s", w.Body.String())
	}
	stored := provider.StoredSearchAPIs()
	if len(stored) != 1 || stored[0].Key != key || !stored[0].Off {
		t.Fatal("search credential and disabled status did not persist")
	}
}
