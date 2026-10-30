package tokenpack

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
)

func TestMCPCallsAreIndependentDurableAndCannotBeRefunded(t *testing.T) {
	sandbox(t)
	p, err := CreateWithMCP("Tools", []string{"plan/m1"}, 10000, time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if p.MCPTotalCalls != 2 || p.MCPRemainingCalls != 2 {
		t.Fatalf("missing independent quota: %+v", p)
	}
	for _, status := range []int{200, 502} {
		id, err := ReserveMCPCall(p.ID, "web_search", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := CompleteMCPCall(id, status); err != nil {
			t.Fatal(err)
		}
		if err := CompleteMCPCall(id, status); err != nil {
			t.Fatal("idempotence", err)
		}
	}
	if _, err := ReserveMCPCall(p.ID, "web_fetch", time.Now()); !errors.Is(err, ErrMCPExhausted) {
		t.Fatalf("quota bypass: %v", err)
	}
	p, err = Get(p.ID)
	if err != nil || p.MCPUsedCalls != 2 || p.MCPRemainingCalls != 0 || p.UsedTokens != 0 || p.ReservedTokens != 0 {
		t.Fatalf("bad balances %+v %v", p, err)
	}
	logs, err := Logs(p.ID, 10)
	if err != nil || len(logs) != 2 {
		t.Fatalf("tool logs %+v %v", logs, err)
	}
	for _, l := range logs {
		if l.Unit != "calls" || l.Calls != 1 || l.TotalTokens != 0 || l.Tool != "web_search" {
			t.Fatalf("invented tokens: %+v", l)
		}
		if _, err := SettleRequest(l.ID, 0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("tool call was token-reconcilable: %v", err)
		}
	}
	lower, higher := int64(1), int64(5)
	if _, err := PatchWithMCP(p.ID, nil, nil, &lower); !errors.Is(err, ErrConflict) {
		t.Fatalf("erased charged calls: %v", err)
	}
	p, err = PatchWithMCP(p.ID, nil, nil, &higher)
	if err != nil || p.MCPUsedCalls != 2 || p.MCPRemainingCalls != 3 {
		t.Fatalf("quota edit reset usage: %+v %v", p, err)
	}
}

func TestLegacyLedgerMigrationPreservesTokensAndCredentials(t *testing.T) {
	sandbox(t)
	secret, err := access.Update("add-pack-key", access.Change{Name: "Legacy", Models: []string{"plan/m1"}})
	if err != nil {
		t.Fatal(err)
	}
	who, ok := access.Authenticate(secret)
	if !ok {
		t.Fatal("missing legacy key")
	}
	if err := os.MkdirAll(filepath.Dir(Path()), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO token_packs(id,name,models,total,used,reserved,enabled,expires,created,share) VALUES(?,'Legacy','["plan/m1"]',1000,300,200,1,0,?,'legacy-share')`, who.KeyID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Get(who.KeyID)
	if err != nil || p.APIKey != secret || p.TotalTokens != 1000 || p.UsedTokens != 300 || p.ReservedTokens != 200 || p.RemainingTokens != 500 || p.MCPTotalCalls != 0 || p.MCPUsedCalls != 0 {
		t.Fatalf("migration lost state: %+v %v", p, err)
	}
	if _, err := ReserveMCPCall(p.ID, "web_fetch", time.Now()); !errors.Is(err, ErrMCPExhausted) {
		t.Fatalf("legacy pack gained tool credits: %v", err)
	}
	calls := int64(2)
	if _, err := PatchWithMCP(p.ID, nil, nil, &calls); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveMCPCall(p.ID, "web_fetch", time.Now()); err != nil {
		t.Fatal(err)
	}
	p, err = Get(p.ID)
	if err != nil || p.MCPUsedCalls != 1 || p.RemainingTokens != 500 || p.ReservedTokens != 200 {
		t.Fatalf("migration mixed units: %+v %v", p, err)
	}
}

func TestMCPCallDisableExpiryAndBrokenLedgerFailClosed(t *testing.T) {
	sandbox(t)
	p, err := CreateWithMCP("Guarded", []string{"plan/m1"}, 1000, time.Now().Add(time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := PatchWithMCP(p.ID, nil, &off, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveMCPCall(p.ID, "web_fetch", time.Now()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	on := true
	if _, err := PatchWithMCP(p.ID, nil, &on, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveMCPCall(p.ID, "web_fetch", time.Now().Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	db, err := openDB(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE token_pack_mcp_requests"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ReserveMCPCall(p.ID, "web_fetch", time.Now()); err == nil {
		t.Fatal("corrupt MCP ledger allowed execution")
	}
	p, err = Get(p.ID)
	if err != nil || p.MCPUsedCalls != 0 || p.MCPRemainingCalls != 2 {
		t.Fatalf("failed admission changed counters: %+v %v", p, err)
	}
}

func TestMCPCallAdmissionIsAtomicAndValidatesBeforeCharge(t *testing.T) {
	sandbox(t)
	p, err := CreateWithMCP("Concurrent tools", []string{"plan/m1"}, 10000, time.Time{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveMCPCall(p.ID, "run_shell", time.Now()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported tool: %v", err)
	}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := ReserveMCPCall(p.ID, "web_fetch", time.Now()); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrMCPExhausted) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	p, err = Get(p.ID)
	if err != nil || admitted.Load() != 3 || p.MCPUsedCalls != 3 || p.UsedTokens != 0 {
		t.Fatalf("concurrent overspend: admitted=%d %+v %v", admitted.Load(), p, err)
	}
}
