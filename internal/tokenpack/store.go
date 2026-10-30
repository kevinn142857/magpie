package tokenpack

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/settings"
	_ "github.com/yetone/magpie/internal/sqlitedb"
)

const schema = `CREATE TABLE IF NOT EXISTS token_packs (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, models TEXT NOT NULL,
 total INTEGER NOT NULL CHECK(total > 0), used INTEGER NOT NULL DEFAULT 0 CHECK(used >= 0),
 reserved INTEGER NOT NULL DEFAULT 0 CHECK(reserved >= 0), enabled INTEGER NOT NULL,
 expires INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL, share TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS token_pack_requests (
 id TEXT PRIMARY KEY, pack_id TEXT NOT NULL REFERENCES token_packs(id),
 created INTEGER NOT NULL, completed INTEGER NOT NULL DEFAULT 0,
 model TEXT NOT NULL, endpoint TEXT NOT NULL, status INTEGER NOT NULL DEFAULT 0,
 tokens INTEGER NOT NULL DEFAULT 0 CHECK(tokens >= 0), held INTEGER NOT NULL CHECK(held >= 0),
 outcome TEXT NOT NULL, message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS token_pack_requests_pack ON token_pack_requests(pack_id,created DESC);`

// Path is the independent package ledger, persisted beside the gateway keys.
func Path() string { return filepath.Join(settings.Dir(), "tokenpacks.db") }

// openDB never recreates a ledger while authorizing a request. A package key
// with a lost ledger therefore fails closed instead of gaining a fresh balance.
func openDB(create bool) (*sql.DB, error) {
	path := Path()
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if create {
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}
	if err := migrateMCP(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return db, nil
}

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type scanner interface{ Scan(...any) error }

const packColumns = "id,name,models,total,used,reserved,enabled,expires,created,share,mcp_total,mcp_used"

func scanPack(row scanner) (Pack, error) {
	var p Pack
	var models string
	var expires, created int64
	if err := row.Scan(&p.ID, &p.Name, &models, &p.TotalTokens, &p.UsedTokens, &p.ReservedTokens, &p.Enabled, &expires, &created, &p.ShareToken, &p.MCPTotalCalls, &p.MCPUsedCalls); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Pack{}, ErrNotFound
		}
		return Pack{}, err
	}
	if err := json.Unmarshal([]byte(models), &p.Models); err != nil {
		return Pack{}, fmt.Errorf("%w: invalid model allow-list", ErrUnavailable)
	}
	clean, err := access.CleanModels(p.Models)
	if err != nil || len(clean) == 0 {
		return Pack{}, fmt.Errorf("%w: invalid model allow-list", ErrUnavailable)
	}
	p.Models = clean
	p.CreatedAt = time.UnixMilli(created).UTC()
	if expires != 0 {
		p.ExpiresAt = time.UnixMilli(expires).UTC()
	}
	p.RemainingTokens = max(p.TotalTokens-p.UsedTokens-p.ReservedTokens, 0)
	p.MCPRemainingCalls = max(p.MCPTotalCalls-p.MCPUsedCalls, 0)
	return p, nil
}

func credential(p Pack) (Pack, error) {
	k, err := access.Update("copy-key", access.Change{Key: p.ID})
	if err != nil {
		return Pack{}, err
	}
	p.APIKey = k
	return p, nil
}

// Create issues a dedicated, model-restricted gateway key and a lifetime
// allowance. A failed creation revokes its unreturned key; any orphan is
// still marked as a package key and cannot bypass its missing ledger entry.
func Create(name string, models []string, quota int64, expiresAt time.Time) (Pack, error) {
	return CreateWithMCP(name, models, quota, expiresAt, 0)
}

// CreateWithMCP creates separate lifetime token and accepted-tool-call quotas.
func CreateWithMCP(name string, models []string, quota int64, expiresAt time.Time, calls int64) (Pack, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 120 {
		return Pack{}, fmt.Errorf("%w: use a package name between 1 and 120 characters", ErrInvalid)
	}
	if quota <= 0 || quota > MaxTokens {
		return Pack{}, fmt.Errorf("%w: token allowance must be a positive exact integer", ErrInvalid)
	}
	if calls < 0 || calls > MaxTokens {
		return Pack{}, fmt.Errorf("%w: MCP call allowance must be a nonnegative exact integer", ErrInvalid)
	}
	clean, err := access.CleanModels(models)
	if err != nil {
		return Pack{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(clean) == 0 {
		return Pack{}, fmt.Errorf("%w: a package must allow at least one model", ErrInvalid)
	}
	now := time.Now().UTC()
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		return Pack{}, fmt.Errorf("%w: expiry must be in the future", ErrInvalid)
	}
	share, err := randomID()
	if err != nil {
		return Pack{}, err
	}
	db, err := openDB(true)
	if err != nil {
		return Pack{}, err
	}
	defer db.Close()
	secret, err := access.Update("add-pack-key", access.Change{Name: name, Models: clean})
	if err != nil {
		return Pack{}, err
	}
	who, ok := access.Authenticate(secret)
	if !ok {
		return Pack{}, errors.New("the new package key could not be read")
	}
	b, _ := json.Marshal(clean)
	expires := int64(0)
	if !expiresAt.IsZero() {
		expires = expiresAt.UnixMilli()
	}
	if _, err := db.Exec(`INSERT INTO token_packs(id,name,models,total,enabled,expires,created,share,mcp_total) VALUES(?,?,?,?,1,?,?,?,?)`, who.KeyID, name, string(b), quota, expires, now.UnixMilli(), share, calls); err != nil {
		_, revokeErr := access.Update("remove-key", access.Change{Key: who.KeyID})
		return Pack{}, errors.Join(err, revokeErr)
	}
	p, err := scanPack(db.QueryRow("SELECT "+packColumns+" FROM token_packs WHERE id=?", who.KeyID))
	if err != nil {
		return Pack{}, err
	}
	p.APIKey = secret
	return p, nil
}

// List returns all packages without their API keys. Sharing tokens are
// administrator-only; callers exposing this list must mask them as appropriate.
func List() ([]Pack, error) {
	db, err := openDB(true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT " + packColumns + " FROM token_packs ORDER BY created DESC,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pack{}
	for rows.Next() {
		p, err := scanPack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func getBy(column, value string) (Pack, error) {
	db, err := openDB(false)
	if err != nil {
		return Pack{}, err
	}
	p, err := scanPack(db.QueryRow("SELECT "+packColumns+" FROM token_packs WHERE "+column+"=?", value))
	db.Close()
	if err != nil {
		return Pack{}, err
	}
	return credential(p)
}

// Get returns a package and its credential for an authenticated administrator.
func Get(id string) (Pack, error) { return getBy("id", id) }

// ByShare resolves an unguessable sharing credential, including inactive
// packages so their recipients can see why new requests are refused.
func ByShare(token string) (Pack, error) { return getBy("share", token) }

// Update changes the lifetime total and enabled state without resetting usage
// or releasing outstanding reservations. quota is the new total, not a top-up.
func Update(id string, quota int64, enabled bool) (Pack, error) {
	return Patch(id, &quota, &enabled)
}

// Patch changes only explicitly supplied fields under a single writer
// transaction. Concurrent allowance and enabled-state edits cannot undo each
// other. It never reads or exposes the underlying API key.
func Patch(id string, quota *int64, enabled *bool) (Pack, error) {
	return PatchWithMCP(id, quota, enabled, nil)
}

// PatchWithMCP atomically edits supplied totals without resetting either usage.
func PatchWithMCP(id string, quota *int64, enabled *bool, calls *int64) (Pack, error) {
	if quota == nil && enabled == nil && calls == nil {
		return Pack{}, fmt.Errorf("%w: supply an allowance or enabled state", ErrInvalid)
	}
	if quota != nil && (*quota <= 0 || *quota > MaxTokens) {
		return Pack{}, fmt.Errorf("%w: token allowance must be a positive exact integer", ErrInvalid)
	}
	if calls != nil && (*calls < 0 || *calls > MaxTokens) {
		return Pack{}, fmt.Errorf("%w: MCP call allowance must be a nonnegative exact integer", ErrInvalid)
	}
	db, err := openDB(false)
	if err != nil {
		return Pack{}, err
	}
	defer db.Close()
	tx, err := beginWrite(db, id)
	if err != nil {
		return Pack{}, err
	}
	defer tx.Rollback()
	p, err := scanPack(tx.QueryRow("SELECT "+packColumns+" FROM token_packs WHERE id=?", id))
	if err != nil {
		return Pack{}, err
	}
	if quota != nil {
		if *quota < p.UsedTokens+p.ReservedTokens {
			return Pack{}, fmt.Errorf("%w: allowance is below consumed and reserved tokens", ErrConflict)
		}
		p.TotalTokens = *quota
	}
	if enabled != nil {
		p.Enabled = *enabled
	}
	if calls != nil {
		if *calls < p.MCPUsedCalls {
			return Pack{}, fmt.Errorf("%w: MCP allowance is below accepted tool calls", ErrConflict)
		}
		p.MCPTotalCalls = *calls
	}
	if _, err := tx.Exec("UPDATE token_packs SET total=?,enabled=?,mcp_total=? WHERE id=?", p.TotalTokens, p.Enabled, p.MCPTotalCalls, id); err != nil {
		return Pack{}, err
	}
	p.RemainingTokens = max(p.TotalTokens-p.UsedTokens-p.ReservedTokens, 0)
	p.MCPRemainingCalls = max(p.MCPTotalCalls-p.MCPUsedCalls, 0)
	if err := tx.Commit(); err != nil {
		return Pack{}, err
	}
	return p, nil
}

// beginWrite acquires SQLite's writer before reading balances, so admission
// remains atomic across goroutines and separate gateway processes.
func beginWrite(db *sql.DB, id string) (*sql.Tx, error) {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE token_packs SET reserved=reserved WHERE id=?", id); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}
