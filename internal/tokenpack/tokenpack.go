// Package tokenpack manages lifetime token allowances independently of usage logs.
package tokenpack

import (
	"errors"
	"time"
)

// Pack is a lifetime allowance. APIKey is returned only by Create, Get and
// ByShare; List omits credentials. ShareToken is itself a bearer credential.
type Pack struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Models          []string  `json:"models"`
	TotalTokens     int64     `json:"totalTokens"`
	UsedTokens      int64     `json:"usedTokens"`
	ReservedTokens  int64     `json:"reservedTokens"`
	RemainingTokens int64     `json:"remainingTokens"`
	Enabled         bool      `json:"enabled"`
	ExpiresAt       time.Time `json:"expiresAt,omitzero"`
	CreatedAt       time.Time `json:"createdAt"`
	ShareToken      string    `json:"shareToken"`
	APIKey          string    `json:"apiKey,omitempty"`
}

// Log contains accounting metadata only; prompts, upstream errors and keys
// are never recorded here. Uncertain requests retain their reservation.
type Log struct {
	ID             string    `json:"id"`
	PackID         string    `json:"packId"`
	CreatedAt      time.Time `json:"createdAt"`
	CompletedAt    time.Time `json:"completedAt,omitzero"`
	Model          string    `json:"model"`
	Endpoint       string    `json:"endpoint"`
	StatusCode     int       `json:"statusCode"`
	TotalTokens    int64     `json:"totalTokens"`
	ReservedTokens int64     `json:"reservedTokens"`
	Outcome        string    `json:"outcome"`
	Message        string    `json:"message"`
}

// Reservation durably holds an allowance until usage is known.
type Reservation struct {
	ID, PackID string
	Tokens     int64
}

var (
	ErrBusy        = errors.New("a request is already using this package")
	ErrUncertain   = errors.New("usage needs administrator settlement")
	ErrDisabled    = errors.New("package is disabled")
	ErrExpired     = errors.New("package has expired")
	ErrModel       = errors.New("model is outside the package")
	ErrExhausted   = errors.New("package has no tokens remaining")
	ErrNotFound    = errors.New("package was not found")
	ErrUnavailable = errors.New("package ledger is unavailable")
	ErrInvalid     = errors.New("invalid package parameters")
	ErrConflict    = errors.New("package accounting conflict")
	ErrUnsupported = errors.New("operation is not supported by token packages")
)

// MaxTokens is the largest exact integer supported by the browser panel.
const MaxTokens int64 = 1<<53 - 1
