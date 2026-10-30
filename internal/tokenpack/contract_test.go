package tokenpack

import (
	"testing"
	"time"
)

func TestNewPackHasLifetimeBalance(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, err := Create("Starter", []string{"plan/m1"}, 1_000_000, time.Time{})
	if err != nil {
		t.Fatal("creating a valid package must succeed:", err)
	}
	if p.TotalTokens != 1_000_000 || p.RemainingTokens != 1_000_000 {
		t.Fatalf("initial balance: %+v", p)
	}
}
