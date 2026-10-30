package tokenpack

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func makePack(t *testing.T, quota int64) Pack {
	t.Helper()
	p, err := Create("Test pack", []string{"plan/m1"}, quota, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreatePackRestrictsItsGatewayKey(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1_000_000)
	if p.ID == "" || p.ShareToken == "" || p.APIKey == "" || p.RemainingTokens != 1_000_000 || !p.Enabled || len(p.Models) != 1 {
		t.Fatalf("incomplete pack: %+v", p)
	}
	who, ok := access.Authenticate(p.APIKey)
	if !ok || who.KeyID != p.ID || !who.Allows("plan/m1") || who.Allows("plan/m2") {
		t.Fatalf("gateway key is not restricted: %+v, %v", who, ok)
	}
	shared, err := ByShare(p.ShareToken)
	if err != nil || shared.ID != p.ID || shared.APIKey != p.APIKey {
		t.Fatalf("share lookup: %+v, %v", shared, err)
	}
	other := makePack(t, 1_000_000)
	if other.ShareToken == p.ShareToken || other.APIKey == p.APIKey {
		t.Fatal("packs reused credentials")
	}
}

func TestInvalidCreationMakesNoKey(t *testing.T) {
	sandbox(t)
	for _, tc := range []struct {
		name    string
		models  []string
		quota   int64
		expires time.Time
	}{
		{"", []string{"plan/m1"}, 1, time.Time{}},
		{"Empty models", nil, 1, time.Time{}},
		{"Bad model", []string{"m1"}, 1, time.Time{}},
		{"No tokens", []string{"plan/m1"}, 0, time.Time{}},
		{"Expired", []string{"plan/m1"}, 1, time.Now().Add(-time.Hour)},
	} {
		if _, err := Create(tc.name, tc.models, tc.quota, tc.expires); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	keys, err := access.List()
	if err != nil || len(keys) != 0 {
		t.Fatalf("invalid creation left keys: %+v, %v", keys, err)
	}
}

func TestReservationSurvivesReopenAndUsageRemoval(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	r, err := Reserve(p.ID, "plan/m1", "/v1/chat/completions", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(p.ID, "plan/m1", "/v1/chat/completions", time.Now()); !errors.Is(err, ErrBusy) {
		t.Fatalf("parallel request: %v", err)
	}
	if _, err := Complete(r.ID, 350, 200, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings.Dir(), "usage.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = Get(p.ID)
	if err != nil || p.UsedTokens != 350 || p.ReservedTokens != 0 || p.RemainingTokens != 650 {
		t.Fatalf("durable balance: %+v, %v", p, err)
	}
	if _, err := Complete(r.ID, 350, 200, true); err != nil {
		t.Fatal("idempotent settlement", err)
	}
	p, _ = Get(p.ID)
	if p.UsedTokens != 350 {
		t.Fatalf("settled twice: %+v", p)
	}
	if _, err := SettleRequest(r.ID, 351); err == nil {
		t.Fatal("rewrote settled usage")
	}
}

func TestConcurrentReservationOnlyOneWinner(t *testing.T) {
	sandbox(t)
	p := makePack(t, 10_000)
	var won atomic.Int64
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now()); err == nil {
				won.Add(1)
			} else if !errors.Is(err, ErrBusy) {
				t.Errorf("reserve: %v", err)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d concurrent requests admitted", won.Load())
	}
	p, err := Get(p.ID)
	if err != nil || p.ReservedTokens != 10_000 || p.RemainingTokens != 0 {
		t.Fatalf("reservation not durable: %+v, %v", p, err)
	}
}

func TestUnknownUsageStaysReservedUntilManualSettlement(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	r, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Complete(r.ID, 20, 502, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("unknown usage was freed: %v", err)
	}
	if _, err := SettleRequest(r.ID, 19); err == nil {
		t.Fatal("settled below known usage")
	}
	if _, err := SettleRequest(r.ID, 60); err != nil {
		t.Fatal(err)
	}
	if _, err := SettleRequest(r.ID, 60); err != nil {
		t.Fatal("manual settlement not idempotent", err)
	}
	p, _ = Get(p.ID)
	if p.UsedTokens != 60 || p.ReservedTokens != 0 || p.RemainingTokens != 940 {
		t.Fatalf("manual settlement: %+v", p)
	}
	logs, err := Logs(p.ID, 100)
	if err != nil || len(logs) != 1 || logs[0].Outcome != "settled" {
		t.Fatalf("logs: %+v, %v", logs, err)
	}
	b, _ := json.Marshal(logs)
	for _, secret := range []string{p.APIKey, p.ShareToken} {
		if len(secret) > 0 && strings.Contains(string(b), secret) {
			t.Fatal("log leaked credential")
		}
	}
}

func TestDisabledExpiredAndMissingLedgerFailClosed(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	if _, err := Update(p.ID, 1000, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := Update(p.ID, 1000, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(p.ID, "plan/other", "/v1/responses", time.Now()); !errors.Is(err, ErrModel) {
		t.Fatalf("model bypass: %v", err)
	}
	expires := time.Now().Add(time.Hour)
	e, err := Create("Expires", []string{"plan/m1"}, 1000, expires)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(e.ID, "plan/m1", "/v1/responses", expires.Add(time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	if err := os.Remove(Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now()); err == nil {
		t.Fatal("missing ledger admitted a package")
	}
}

func TestAdministratorCannotSettleAnExecutingRequest(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	r, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SettleRequest(r.ID, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("live request was manually released: %v", err)
	}
	if _, err := Complete(r.ID, 0, 502, false); err != nil {
		t.Fatal(err)
	}
	if _, err := SettleRequest(r.ID, 20); err != nil {
		t.Fatal("finished uncertain request cannot be reconciled:", err)
	}
}

func TestPackagePatchDoesNotOverwriteUnspecifiedFields(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	off := false
	if _, err := Patch(p.ID, nil, &off); err != nil {
		t.Fatal("disabling a package:", err)
	}
	quota := int64(2000)
	p, err := Patch(p.ID, &quota, nil)
	if err != nil || p.Enabled || p.TotalTokens != 2000 {
		t.Fatalf("allowance change overwrote enabled state: %+v %v", p, err)
	}
	if _, err := Patch(p.ID, nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty patch: %v", err)
	}
	if _, err := Patch("missing", &quota, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing package: %v", err)
	}
}

func TestConcurrentDisableAndAllowancePatchBothSurvive(t *testing.T) {
	sandbox(t)
	for range 10 {
		p := makePack(t, 1000)
		off := false
		quota := int64(2000)
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := Patch(p.ID, nil, &off); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := Patch(p.ID, &quota, nil); err != nil {
				t.Error(err)
			}
		})
		wg.Wait()
		p, err := Get(p.ID)
		if err != nil || p.Enabled || p.TotalTokens != 2000 {
			t.Fatalf("lost concurrent patch: %+v %v", p, err)
		}
	}
}

func TestLogsWithoutFilterIncludeEveryPackage(t *testing.T) {
	sandbox(t)
	for range 2 {
		p := makePack(t, 1000)
		r, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Complete(r.ID, 50, 200, true); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := Logs("", 100)
	if err != nil || len(logs) != 2 || logs[0].PackID == logs[1].PackID {
		t.Fatalf("all-package logs: %+v %v", logs, err)
	}
}

func TestRefusalLogCannotReleaseAllowanceOrStoreUntrustedFields(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	r, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordRefusal(p.ID, p.APIKey, "/unsupported/"+p.APIKey, 409, ErrBusy); err != nil {
		t.Fatal("recording refusal:", err)
	}
	p, err = Get(p.ID)
	if err != nil || p.ReservedTokens != 1000 || p.UsedTokens != 0 {
		t.Fatalf("refusal changed reservation: %+v %v", p, err)
	}
	entries, err := Logs(p.ID, 100)
	if err != nil || len(entries) != 2 {
		t.Fatalf("refusal logs: %+v %v", entries, err)
	}
	b, _ := json.Marshal(entries)
	if strings.Contains(string(b), p.APIKey) || strings.Contains(string(b), p.ShareToken) {
		t.Fatal("refusal persisted credentials")
	}
	found := false
	for _, e := range entries {
		if e.Outcome == "rejected" {
			found = true
			if e.StatusCode != 409 || e.TotalTokens != 0 || e.ReservedTokens != 0 {
				t.Fatalf("refusal metadata: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("no rejection row")
	}
	if _, err := Complete(r.ID, 100, 200, true); err != nil {
		t.Fatal(err)
	}
}

func TestExceededAllowanceCanStillBeDisabled(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	r, err := Reserve(p.ID, "plan/m1", "/v1/responses", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Complete(r.ID, 1200, 200, true); err != nil {
		t.Fatal(err)
	}
	off := false
	p, err = Patch(p.ID, nil, &off)
	if err != nil || p.Enabled || p.TotalTokens != 1000 || p.UsedTokens != 1200 {
		t.Fatalf("cannot disable overspent package: %+v %v", p, err)
	}
}

func TestCorruptedPackageModelAllowListFailsClosed(t *testing.T) {
	sandbox(t)
	p := makePack(t, 1000)
	db, err := sql.Open("sqlite", Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, models := range []string{"null", "[]", `["bad-model"]`} {
		if _, err := db.Exec("UPDATE token_packs SET models=? WHERE id=?", models, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := Get(p.ID); err == nil {
			t.Fatalf("corrupt model list became unrestricted: %s", models)
		}
	}
}
