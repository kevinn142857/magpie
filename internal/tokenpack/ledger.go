package tokenpack

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/access"
)

const logColumns = "id,pack_id,created,completed,model,endpoint,status,tokens,held,outcome,message"

// RecordRefusal records a request rejected before any provider was contacted.
func RecordRefusal(id, model, endpoint string, statusCode int, reason error) error {
	db, err := openDB(false)
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := scanPack(db.QueryRow("SELECT "+packColumns+" FROM token_packs WHERE id=?", id))
	if err != nil {
		return err
	}
	// Unrecognized model and path strings can contain pasted credentials.
	// Keep only an allowed model and a fixed route label, never raw input.
	if len(model) > 300 || !access.ModelAllowed(p.Models, model) {
		model = ""
	}
	message := "Request refused before upstream."
	switch {
	case errors.Is(reason, ErrBusy):
		message = "Another request is still in flight."
	case errors.Is(reason, ErrUncertain):
		message = "Usage is uncertain; administrator reconciliation required."
	case errors.Is(reason, ErrExhausted):
		message = "Lifetime token allowance exhausted."
	case errors.Is(reason, ErrDisabled):
		message = "Package is disabled."
	case errors.Is(reason, ErrExpired):
		message = "Package has expired."
	case errors.Is(reason, ErrModel):
		message = "Requested model is outside this package."
	case errors.Is(reason, ErrUnsupported):
		message = "Operation is outside this package's supported text APIs."
	case errors.Is(reason, ErrInvalid):
		message = "Invalid request; provider was not contacted."
	}
	logID, err := randomID()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = db.Exec(`INSERT INTO token_pack_requests(id,pack_id,created,completed,model,endpoint,status,held,outcome,message) VALUES(?,?,?,?,?,?,?,0,'rejected',?)`, logID, id, now, now, model, safeEndpoint(endpoint), statusCode, message)
	return err
}

func safeEndpoint(path string) string {
	switch path {
	case "/v1/chat/completions", "/chat/completions", "/v1/responses", "/responses", "/v1/messages", "/messages", "/v1/models", "/models", "/v1beta/models":
		return path
	}
	if strings.HasPrefix(path, "/v1beta/models/") {
		if strings.HasSuffix(path, ":streamGenerateContent") {
			return "/v1beta/models/:streamGenerateContent"
		}
		if strings.HasSuffix(path, ":generateContent") {
			return "/v1beta/models/:generateContent"
		}
	}
	if strings.HasPrefix(path, "/v1/models/") {
		return "/v1/models/:model"
	}
	return "unsupported"
}

// executing prevents an administrator from releasing a request still served
// by this process. After a crash, the durable pending row remains reconcilable.
var executing sync.Map

func executionID(id string) string { return Path() + "\x00" + id }

func scanLog(row scanner) (Log, error) {
	var l Log
	var created, completed int64
	if err := row.Scan(&l.ID, &l.PackID, &created, &completed, &l.Model, &l.Endpoint, &l.StatusCode, &l.TotalTokens, &l.ReservedTokens, &l.Outcome, &l.Message); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Log{}, ErrNotFound
		}
		return Log{}, err
	}
	l.CreatedAt = time.UnixMilli(created).UTC()
	if completed != 0 {
		l.CompletedAt = time.UnixMilli(completed).UTC()
	}
	return l, nil
}

// Logs returns the newest package requests, including unresolved reservations.
// An empty id selects every package for an authenticated management view.
func Logs(id string, limit int) ([]Log, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	db, err := openDB(false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT "+logColumns+" FROM token_pack_requests WHERE (?='' OR pack_id=?) ORDER BY created DESC,id LIMIT ?", id, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Log{}
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Reserve holds all currently available tokens before contacting an upstream.
// This deliberately admits one request per package at a time. Reservations
// survive crashes; uncertain usage is never treated as free consumption.
func Reserve(id, model, endpoint string, now time.Time) (Reservation, error) {
	db, err := openDB(false)
	if err != nil {
		return Reservation{}, err
	}
	defer db.Close()
	tx, err := beginWrite(db, id)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback()
	p, err := scanPack(tx.QueryRow("SELECT "+packColumns+" FROM token_packs WHERE id=?", id))
	if err != nil {
		return Reservation{}, err
	}
	switch {
	case !p.Enabled:
		return Reservation{}, ErrDisabled
	case !p.ExpiresAt.IsZero() && !now.Before(p.ExpiresAt):
		return Reservation{}, ErrExpired
	case !access.ModelAllowed(p.Models, model):
		return Reservation{}, ErrModel
	case p.UsedTokens >= p.TotalTokens:
		return Reservation{}, ErrExhausted
	case p.ReservedTokens > 0:
		var unknown int
		if err := tx.QueryRow("SELECT COUNT(*) FROM token_pack_requests WHERE pack_id=? AND outcome='uncertain'", id).Scan(&unknown); err != nil {
			return Reservation{}, err
		}
		if unknown > 0 {
			return Reservation{}, ErrUncertain
		}
		return Reservation{}, ErrBusy
	}
	requestID, err := randomID()
	if err != nil {
		return Reservation{}, err
	}
	if _, err := tx.Exec("UPDATE token_packs SET reserved=reserved+? WHERE id=?", p.RemainingTokens, id); err != nil {
		return Reservation{}, err
	}
	if _, err := tx.Exec(`INSERT INTO token_pack_requests(id,pack_id,created,model,endpoint,held,outcome,message) VALUES(?,?,?,?,?,?,'pending','Request admitted; allowance reserved.')`, requestID, id, now.UnixMilli(), model, endpoint, p.RemainingTokens); err != nil {
		return Reservation{}, err
	}
	executing.Store(executionID(requestID), true)
	if err := tx.Commit(); err != nil {
		executing.Delete(executionID(requestID))
		return Reservation{}, err
	}
	return Reservation{ID: requestID, PackID: id, Tokens: p.RemainingTokens}, nil
}

// Complete settles known usage, or marks a request uncertain while keeping its
// reservation. tokens may contain a known partial count for later reconciliation.
func Complete(id string, tokens int64, statusCode int, certain bool) (Log, error) {
	defer executing.Delete(executionID(id))
	return settle(id, tokens, statusCode, certain, false)
}

// SettleRequest reconciles a pending or uncertain request using an administrator's
// verified total. It never allows known usage to be reduced; a repeated identical
// reconciliation is idempotent. Stop the affected client before reconciling a
// pending request that could still be executing in another process.
func SettleRequest(id string, actualTokens int64) (Log, error) {
	if _, active := executing.Load(executionID(id)); active {
		return Log{}, ErrBusy
	}
	return settle(id, actualTokens, 0, true, true)
}

func settle(id string, tokens int64, statusCode int, certain, manual bool) (Log, error) {
	if tokens < 0 || tokens > MaxTokens {
		return Log{}, fmt.Errorf("%w: usage must be a nonnegative exact integer", ErrInvalid)
	}
	db, err := openDB(false)
	if err != nil {
		return Log{}, err
	}
	defer db.Close()
	// The first write serializes settlement with reservation and allowance edits.
	tx, err := db.Begin()
	if err != nil {
		return Log{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE token_pack_requests SET tokens=tokens WHERE id=?", id); err != nil {
		return Log{}, err
	}
	l, err := scanLog(tx.QueryRow("SELECT "+logColumns+" FROM token_pack_requests WHERE id=?", id))
	if err != nil {
		return Log{}, err
	}
	if l.Outcome == "settled" {
		if certain && l.TotalTokens == tokens {
			return l, nil
		}
		return Log{}, fmt.Errorf("%w: request has already been settled with a different total", ErrConflict)
	}
	if l.Outcome != "pending" && l.Outcome != "uncertain" {
		return Log{}, fmt.Errorf("%w: request cannot be settled", ErrConflict)
	}
	if tokens < l.TotalTokens {
		return Log{}, fmt.Errorf("%w: usage cannot be less than the count already recorded", ErrInvalid)
	}
	if statusCode == 0 {
		statusCode = l.StatusCode
	}
	outcome, message := "uncertain", "Usage incomplete; reservation retained for administrator review."
	if certain {
		outcome, message = "settled", "Usage recorded."
		if manual {
			message = "Administrator reconciled usage."
		}
		result, err := tx.Exec("UPDATE token_packs SET used=used+?,reserved=reserved-? WHERE id=? AND reserved>=?", tokens, l.ReservedTokens, l.PackID, l.ReservedTokens)
		if err != nil {
			return Log{}, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return Log{}, fmt.Errorf("%w: reservation does not match its package", ErrConflict)
		}
	}
	if _, err := tx.Exec("UPDATE token_pack_requests SET completed=?,status=?,tokens=?,outcome=?,message=? WHERE id=?", time.Now().UnixMilli(), statusCode, tokens, outcome, message, id); err != nil {
		return Log{}, err
	}
	l, err = scanLog(tx.QueryRow("SELECT "+logColumns+" FROM token_pack_requests WHERE id=?", id))
	if err != nil {
		return Log{}, err
	}
	if err := tx.Commit(); err != nil {
		return Log{}, err
	}
	return l, nil
}
