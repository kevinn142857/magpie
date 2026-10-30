package tokenpack

import (
	"database/sql"
	"fmt"
	"time"
)

func migrateMCP(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version >= 1 {
		return err
	}
	tx, err := beginWrite(db, "")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 0 {
		for _, statement := range []string{
			"ALTER TABLE token_packs ADD COLUMN mcp_total INTEGER NOT NULL DEFAULT 0 CHECK(mcp_total >= 0)",
			"ALTER TABLE token_packs ADD COLUMN mcp_used INTEGER NOT NULL DEFAULT 0 CHECK(mcp_used >= 0)",
			`CREATE TABLE token_pack_mcp_requests (id TEXT PRIMARY KEY,pack_id TEXT NOT NULL REFERENCES token_packs(id),created INTEGER NOT NULL,completed INTEGER NOT NULL DEFAULT 0,tool TEXT NOT NULL,status INTEGER NOT NULL DEFAULT 0,outcome TEXT NOT NULL,message TEXT NOT NULL)`,
			"CREATE INDEX token_pack_mcp_requests_pack ON token_pack_mcp_requests(pack_id,created DESC)",
			"PRAGMA user_version=1",
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ReserveMCPCall durably charges one accepted tool call before dispatch.
func ReserveMCPCall(id, tool string, now time.Time) (string, error) {
	if tool != "web_search" && tool != "web_fetch" {
		return "", ErrUnsupported
	}
	db, err := openDB(false)
	if err != nil {
		return "", err
	}
	defer db.Close()
	tx, err := beginWrite(db, id)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	p, err := scanPack(tx.QueryRow("SELECT "+packColumns+" FROM token_packs WHERE id=?", id))
	if err != nil {
		return "", err
	}
	switch {
	case !p.Enabled:
		return "", ErrDisabled
	case !p.ExpiresAt.IsZero() && !now.Before(p.ExpiresAt):
		return "", ErrExpired
	case p.MCPUsedCalls >= p.MCPTotalCalls:
		return "", ErrMCPExhausted
	}
	requestID, err := randomID()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec("UPDATE token_packs SET mcp_used=mcp_used+1 WHERE id=?", id); err != nil {
		return "", err
	}
	if _, err := tx.Exec("INSERT INTO token_pack_mcp_requests(id,pack_id,created,tool,outcome,message) VALUES(?,?,?,?,'pending','Tool call accepted; one call charged.')", requestID, id, now.UnixMilli(), tool); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return requestID, nil
}

// CompleteMCPCall records an accepted tool's outcome without refunding its call.
func CompleteMCPCall(id string, status int) error {
	if status < 100 || status > 599 {
		return fmt.Errorf("%w: invalid status", ErrInvalid)
	}
	db, err := openDB(false)
	if err != nil {
		return err
	}
	defer db.Close()
	outcome, message := "settled", "Tool call completed; one accepted call charged."
	if status >= 400 {
		outcome, message = "failed", "Tool call failed after admission; one accepted call charged."
	}
	result, err := db.Exec("UPDATE token_pack_mcp_requests SET completed=?,status=?,outcome=?,message=? WHERE id=? AND completed=0", time.Now().UnixMilli(), status, outcome, message, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n > 0 {
		return err
	}
	var exists int
	if err := db.QueryRow("SELECT COUNT(*) FROM token_pack_mcp_requests WHERE id=?", id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	return nil
}

func mcpLogs(db *sql.DB, id string, limit int) ([]Log, error) {
	rows, err := db.Query("SELECT id,pack_id,created,completed,tool,status,outcome,message FROM token_pack_mcp_requests WHERE (?='' OR pack_id=?) ORDER BY created DESC,id LIMIT ?", id, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Log{}
	for rows.Next() {
		l := Log{Endpoint: "/mcp/magpie/package", Unit: "calls", Calls: 1}
		var created, completed int64
		if err := rows.Scan(&l.ID, &l.PackID, &created, &completed, &l.Tool, &l.StatusCode, &l.Outcome, &l.Message); err != nil {
			return nil, err
		}
		l.CreatedAt = time.UnixMilli(created).UTC()
		if completed != 0 {
			l.CompletedAt = time.UnixMilli(completed).UTC()
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
