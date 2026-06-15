// Package ledger provides double-entry accounting primitives for Maniflex
// applications.
//
// # Quick start
//
//	server.MustRegister(ledger.Models()...)
//	l := ledger.New(server)
//
//	// Inside a Service middleware:
//	entry, err := l.Post(ctx,
//	    ledger.EntryInput{Description: "Sale invoice #1001", PeriodID: "2026-05", Date: time.Now()},
//	    []ledger.LineInput{
//	        {AccountID: "ar-001", Debit: money.New(100_00, "SAR")},
//	        {AccountID: "rev-001", Credit: money.New(100_00, "SAR")},
//	    },
//	)
//
// # Models
//
// The package provides three Maniflex model types that become standard REST
// endpoints when registered:
//
//   - LedgerAccount — chart of accounts
//   - LedgerEntry   — journal entry header (read-only once posted)
//   - LedgerLine    — individual debit or credit leg
//
// # Balance queries
//
//	bal, err := ledger.Balance(ctx, "ar-001", "SAR", time.Now())
//	tb, err  := ledger.TrialBalance(ctx, "SAR", time.Now())
package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/money"
)

// Sentinel errors returned by Post.
var (
	// ErrImbalanced is returned when the sum of debit cents does not equal the
	// sum of credit cents for any currency represented in the entry.
	ErrImbalanced = errors.New("ledger: entry is not balanced")

	// ErrNoLines is returned when Post is called with fewer than two lines.
	ErrNoLines = errors.New("ledger: entry must have at least two lines")
)

// LedgerAccount is a chart-of-accounts entry. Register it via Models().
type LedgerAccount struct {
	maniflex.BaseModel
	Code     string `json:"code"      db:"code"      mfx:"required,unique,filterable,sortable"`
	Name     string `json:"name"      db:"name"      mfx:"required,filterable,sortable,searchable"`
	Type     string `json:"type"      db:"type"      mfx:"required,filterable,enum:asset|liability|equity|revenue|expense"`
	ParentID string `json:"parent_id" db:"parent_id" mfx:"filterable"`
}

// LedgerEntry is a journal entry header. PostedAt is set automatically by
// Post() and is read-only via the REST API.
type LedgerEntry struct {
	maniflex.BaseModel
	Description string     `json:"description" db:"description" mfx:"required,filterable,searchable"`
	Reference   string     `json:"reference"   db:"reference"   mfx:"filterable"`
	PeriodID    string     `json:"period_id"   db:"period_id"   mfx:"required,filterable"`
	Date        time.Time  `json:"date"        db:"date"        mfx:"required,filterable,sortable"`
	PostedAt    *time.Time `json:"posted_at"   db:"posted_at"   mfx:"readonly,filterable,sortable"`
}

// LedgerLine is one leg of a journal entry. Exactly one of DebitCents or
// CreditCents is non-zero.
type LedgerLine struct {
	maniflex.BaseModel
	EntryID     string `json:"entry_id"     db:"entry_id"     mfx:"required,filterable"`
	AccountID   string `json:"account_id"   db:"account_id"   mfx:"required,filterable"`
	DebitCents  int64  `json:"debit_cents"  db:"debit_cents"  mfx:"filterable,sortable"`
	CreditCents int64  `json:"credit_cents" db:"credit_cents" mfx:"filterable,sortable"`
	Currency    string `json:"currency"     db:"currency"     mfx:"required,filterable"`
	Notes       string `json:"notes"        db:"notes"        mfx:"filterable"`
}

// EntryInput carries the journal entry header for a Post call.
type EntryInput struct {
	Description string
	Reference   string    // optional memo / source document reference
	PeriodID    string    // e.g. "2026-05" or "2026-Q2" — opaque to pkg/ledger
	Date        time.Time // accounting date (may differ from PostedAt)
}

// LineInput is one debit or credit leg. Set Debit or Credit — not both.
// Currency is taken from whichever Amount is non-zero.
type LineInput struct {
	AccountID string
	Debit     money.Amount // set for debit legs; leave zero for credit legs
	Credit    money.Amount // set for credit legs; leave zero for debit legs
	Notes     string       // optional free-text annotation
}

// AccountBalance is one row in a TrialBalance result.
type AccountBalance struct {
	AccountID string
	Currency  string
	// Balance is positive for a net debit balance, negative for a net credit
	// balance. Whether a positive or negative balance is "normal" depends on
	// the account type (asset/expense accounts have normal debit balances;
	// liability/equity/revenue have normal credit balances).
	Balance money.Amount
}

// Models returns the three model types to pass to server.MustRegister.
//
//	server.MustRegister(ledger.Models()...)
//	l := ledger.New(server)
func Models() []any {
	return []any{LedgerAccount{}, LedgerEntry{}, LedgerLine{}}
}

// Ledger provides the Post operation.
type Ledger struct{}

// New returns a Ledger. The models from Models() must be registered on the
// server before calling Post.
func New() *Ledger { return &Ledger{} }

// Post records a balanced journal entry atomically.
//
// Validation (performed before any DB work):
//   - At least two lines are required.
//   - Each line must have exactly one non-zero amount (Debit or Credit).
//   - Amounts must be non-negative.
//   - Each line must specify an AccountID and a currency (via the non-zero Amount).
//   - Sum of debits must equal sum of credits per currency.
//
// If ctx.Tx is already set, Post joins the active transaction and does not
// commit — the caller is responsible. Otherwise Post opens and commits its own
// transaction; if any step fails the transaction is rolled back.
//
// PostedAt on the returned entry is set to time.Now().UTC().
func (l *Ledger) Post(ctx *maniflex.ServerContext, entry EntryInput, lines []LineInput) (*LedgerEntry, error) {
	if len(lines) < 2 {
		return nil, ErrNoLines
	}
	if err := validateLines(lines); err != nil {
		return nil, err
	}
	if err := validateBalance(lines); err != nil {
		return nil, err
	}

	ownTx := ctx.Tx == nil
	if ownTx {
		tx, err := ctx.BeginTx(ctx.Ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("ledger: begin tx: %w", err)
		}
		ctx.Tx = tx
	}

	result, writeErr := l.writeEntryAndLines(ctx, entry, lines)
	if writeErr != nil {
		if ownTx {
			ctx.Tx.Rollback()
			ctx.Tx = nil
		}
		return nil, writeErr
	}

	if ownTx {
		if err := ctx.Tx.Commit(); err != nil {
			ctx.Tx.Rollback()
			ctx.Tx = nil
			return nil, fmt.Errorf("ledger: commit: %w", err)
		}
		ctx.Tx = nil
	}
	return result, nil
}

// writeEntryAndLines inserts the entry header and all line rows. It assumes
// ctx.Tx is already set (either by the caller or by Post). On any error the
// caller is responsible for rolling back the transaction.
func (l *Ledger) writeEntryAndLines(ctx *maniflex.ServerContext, entry EntryInput, lines []LineInput) (*LedgerEntry, error) {
	now := time.Now().UTC()
	created, err := maniflex.Create(ctx, &LedgerEntry{
		Description: entry.Description,
		Reference:   entry.Reference,
		PeriodID:    entry.PeriodID,
		Date:        entry.Date,
		PostedAt:    &now,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: insert entry: %w", err)
	}
	if err := insertLines(ctx, created.ID, lines); err != nil {
		return nil, err
	}
	return created, nil
}

// insertLines writes one LedgerLine row per LineInput.
func insertLines(ctx *maniflex.ServerContext, entryID string, lines []LineInput) error {
	for i, li := range lines {
		_, err := maniflex.Create(ctx, &LedgerLine{
			EntryID:     entryID,
			AccountID:   li.AccountID,
			DebitCents:  li.Debit.Cents,
			CreditCents: li.Credit.Cents,
			Currency:    lineCurrency(li),
			Notes:       li.Notes,
		})
		if err != nil {
			return fmt.Errorf("ledger: insert line %d: %w", i, err)
		}
	}
	return nil
}

// ── Balance queries ───────────────────────────────────────────────────────────

// Balance returns the net balance of accountID in the given currency as of
// asOf. Positive = net debit balance; negative = net credit balance.
//
// ctx.Tx is honoured: when an active transaction is set, the query runs within it.
func Balance(ctx *maniflex.ServerContext, accountID, currency string, asOf time.Time) (money.Amount, error) {
	p := newPH(ctx.DriverType())
	query := fmt.Sprintf(
		`SELECT COALESCE(SUM(ll.debit_cents), 0) - COALESCE(SUM(ll.credit_cents), 0) AS balance
		 FROM ledger_lines ll
		 JOIN ledger_entries le ON le.id = ll.entry_id
		 WHERE ll.account_id = %s
		   AND ll.currency = %s
		   AND le.date <= %s`,
		p.next(accountID), p.next(currency), p.next(asOf.UTC().Format(time.RFC3339)),
	)
	rows, err := ctx.RawQuery(query, p.args...)
	if err != nil {
		return money.Amount{}, fmt.Errorf("ledger: balance: %w", err)
	}
	if len(rows) == 0 {
		return money.New(0, currency), nil
	}
	return money.New(toInt64(rows[0]["balance"]), currency), nil
}

// TrialBalance returns the net balance of every account that has any activity
// in the given currency as of asOf, ordered by account_id.
func TrialBalance(ctx *maniflex.ServerContext, currency string, asOf time.Time) ([]AccountBalance, error) {
	p := newPH(ctx.DriverType())
	query := fmt.Sprintf(
		`SELECT ll.account_id,
		        COALESCE(SUM(ll.debit_cents), 0) - COALESCE(SUM(ll.credit_cents), 0) AS balance
		 FROM ledger_lines ll
		 JOIN ledger_entries le ON le.id = ll.entry_id
		 WHERE ll.currency = %s
		   AND le.date <= %s
		 GROUP BY ll.account_id
		 ORDER BY ll.account_id`,
		p.next(currency), p.next(asOf.UTC().Format(time.RFC3339)),
	)
	rows, err := ctx.RawQuery(query, p.args...)
	if err != nil {
		return nil, fmt.Errorf("ledger: trial balance: %w", err)
	}
	out := make([]AccountBalance, 0, len(rows))
	for _, row := range rows {
		accID, _ := row["account_id"].(string)
		out = append(out, AccountBalance{
			AccountID: accID,
			Currency:  currency,
			Balance:   money.New(toInt64(row["balance"]), currency),
		})
	}
	return out, nil
}

// ── Placeholder builder ───────────────────────────────────────────────────────

type phBuilder struct {
	driver maniflex.DriverType
	args   []any
	n      int
}

func newPH(driver maniflex.DriverType) *phBuilder {
	return &phBuilder{driver: driver}
}

func (p *phBuilder) next(v any) string {
	p.args = append(p.args, v)
	p.n++
	if p.driver == maniflex.Postgres {
		return fmt.Sprintf("$%d", p.n)
	}
	return "?"
}

// ── Validation ────────────────────────────────────────────────────────────────

func validateLines(lines []LineInput) error {
	for i, l := range lines {
		if l.AccountID == "" {
			return fmt.Errorf("ledger: line %d: account_id is required", i)
		}
		if l.Debit.Cents < 0 || l.Credit.Cents < 0 {
			return fmt.Errorf("ledger: line %d: amounts must be non-negative", i)
		}
		if l.Debit.Cents != 0 && l.Credit.Cents != 0 {
			return fmt.Errorf("ledger: line %d: cannot have both debit and credit", i)
		}
		if l.Debit.Cents == 0 && l.Credit.Cents == 0 {
			return fmt.Errorf("ledger: line %d: debit and credit are both zero", i)
		}
		if lineCurrency(l) == "" {
			return fmt.Errorf("ledger: line %d: currency is required", i)
		}
	}
	return nil
}

func validateBalance(lines []LineInput) error {
	debits := make(map[string]int64)
	credits := make(map[string]int64)
	for _, l := range lines {
		cur := lineCurrency(l)
		debits[cur] += l.Debit.Cents
		credits[cur] += l.Credit.Cents
	}
	for cur, d := range debits {
		if d != credits[cur] {
			return fmt.Errorf("%w: currency %s: debits %d ≠ credits %d",
				ErrImbalanced, cur, d, credits[cur])
		}
	}
	// Currencies appearing only in credits (no corresponding debit key).
	for cur, cr := range credits {
		if _, ok := debits[cur]; !ok && cr != 0 {
			return fmt.Errorf("%w: currency %s: debits 0 ≠ credits %d",
				ErrImbalanced, cur, cr)
		}
	}
	return nil
}

// lineCurrency returns the ISO 4217 code of the non-zero amount on a LineInput.
func lineCurrency(l LineInput) string {
	if l.Debit.Cents != 0 {
		return l.Debit.Currency
	}
	return l.Credit.Currency
}

// ── Helpers ───────────────────────────────────────────────────────────────────
//
// mapToEntry/asTime were removed when Post() moved to the typed maniflex.Create
// helper, which returns a fully-scanned *LedgerEntry directly.

// toInt64 converts the database driver types that SQL aggregate functions
// may return — int64, float64, []byte, int, or string — to int64. Postgres
// returns SUM() over a BIGINT column as NUMERIC, which lib/pq surfaces as a
// string (the row scanner turns the driver's []byte into a string), so the
// string case is required for the Postgres lane.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case []byte:
		var i int64
		fmt.Sscan(string(n), &i)
		return i
	case string:
		var i int64
		fmt.Sscan(n, &i)
		return i
	}
	return 0
}
