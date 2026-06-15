// Package money provides a fixed-precision monetary amount type for use as a
// model field. It implements maniflex.SQLTyper so AutoMigrate produces a
// NUMERIC(19,4) column on Postgres and a TEXT column on SQLite.
//
// Values are stored as a decimal string with 4 decimal places ("12.3400").
// Internally, amounts are represented as an integer number of minor currency
// units (cents, halala, fils, etc.) plus an ISO 4217 currency code.
//
// Arithmetic methods (Add, Sub, Mul) are exact — all operations stay in
// integer space and never touch floating point.
package money

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"maniflex"
)

// ErrCurrencyMismatch is returned by Add and Sub when the two amounts carry
// different currency codes.
var ErrCurrencyMismatch = errors.New("currency mismatch")

// Amount is a monetary value stored as an integer number of minor currency
// units (100 units = 1 major unit for most currencies). The SQL column type is
// NUMERIC(19,4) on Postgres and TEXT on SQLite, both holding the decimal form
// (e.g. "12.3400" for 1234 cents).
type Amount struct {
	Cents    int64  `json:"amount"`
	Currency string `json:"currency"`
}

// SQLType implements maniflex.SQLTyper.
func (Amount) SQLType(d maniflex.DriverType) string {
	if d == maniflex.Postgres {
		return "NUMERIC(19,4)"
	}
	return "TEXT"
}

// Value implements driver.Valuer so database/sql stores Amount as a decimal
// string ("12.3400"). A zero Amount stores as "0.0000".
func (a Amount) Value() (driver.Value, error) {
	return a.format(), nil
}

// Scan implements sql.Scanner so database/sql can populate an Amount from a
// NUMERIC or TEXT column. Currency is left empty; callers must populate it
// from a separate column if needed.
func (a *Amount) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return a.parseDecimal(v)
	case []byte:
		return a.parseDecimal(string(v))
	case float64:
		return a.parseDecimal(strconv.FormatFloat(v, 'f', 4, 64))
	case int64:
		a.Cents = v * 100
		return nil
	case nil:
		a.Cents = 0
		return nil
	default:
		return fmt.Errorf("money: cannot scan %T into Amount", src)
	}
}

// MarshalJSON encodes as {"amount": <cents>, "currency": "<code>"}.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}{a.Cents, a.Currency})
}

// UnmarshalJSON decodes {"amount": <cents>, "currency": "<code>"}.
func (a *Amount) UnmarshalJSON(b []byte) error {
	var v struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	a.Cents = v.Amount
	a.Currency = v.Currency
	return nil
}

// Add returns the sum of a and b. Returns ErrCurrencyMismatch if their
// currency codes differ.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := sameCurrency(a, b); err != nil {
		return Amount{}, err
	}
	return Amount{Cents: a.Cents + b.Cents, Currency: a.Currency}, nil
}

// Sub returns a minus b. Returns ErrCurrencyMismatch if their currency codes
// differ.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := sameCurrency(a, b); err != nil {
		return Amount{}, err
	}
	return Amount{Cents: a.Cents - b.Cents, Currency: a.Currency}, nil
}

// Mul scales a by the exact rational num/denom using integer arithmetic.
// denom must not be zero.
func (a Amount) Mul(num, denom int64) Amount {
	return Amount{Cents: a.Cents * num / denom, Currency: a.Currency}
}

// IsZero reports whether the amount is zero.
func (a Amount) IsZero() bool { return a.Cents == 0 }

// New constructs an Amount from cents and a currency code.
func New(cents int64, currency string) Amount {
	return Amount{Cents: cents, Currency: currency}
}

// ── internal ──────────────────────────────────────────────────────────────────

// format returns the 4-decimal-place decimal string used for SQL storage.
func (a Amount) format() string {
	whole := a.Cents / 100
	frac := a.Cents % 100
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%d.%02d00", whole, frac)
}

// parseDecimal parses a decimal string like "12.3400" into Cents.
func (a *Amount) parseDecimal(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		a.Cents = 0
		return nil
	}
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return fmt.Errorf("money: parse %q: %w", s, err)
	}
	var frac int64
	if len(parts) == 2 {
		fs := parts[1]
		// Normalise to exactly 2 significant decimal places.
		if len(fs) > 2 {
			fs = fs[:2]
		}
		for len(fs) < 2 {
			fs += "0"
		}
		frac, err = strconv.ParseInt(fs, 10, 64)
		if err != nil {
			return fmt.Errorf("money: parse %q: %w", s, err)
		}
	}
	if whole < 0 {
		a.Cents = whole*100 - frac
	} else {
		a.Cents = whole*100 + frac
	}
	return nil
}

func sameCurrency(a, b Amount) error {
	if a.Currency != b.Currency {
		return fmt.Errorf("%w: %q vs %q", ErrCurrencyMismatch, a.Currency, b.Currency)
	}
	return nil
}
