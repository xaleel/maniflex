package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"maniflex"
	"maniflex/pkg/ledger"
	"maniflex/pkg/money"
	"maniflex/tests/e2e/testutil"
)

// TestLedger covers roadmap item 5.13 — pkg/ledger double-entry primitives.
//
// Run this group specifically:
//
//	go test ./tests/e2e/... -run TestLedger

// ── shared helpers ────────────────────────────────────────────────────────────

type ledgerFixture struct {
	srv *testutil.Server
	l   *ledger.Ledger
}

func newLedgerFixture(t *testing.T) ledgerFixture {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{
		Models: ledger.Models(),
	})
	srv.POST("/ledger_accounts", map[string]any{"code": "AR-001",  "name": "Accounts Receivable", "type": "asset"}).AssertStatus(http.StatusCreated)
	srv.POST("/ledger_accounts", map[string]any{"code": "REV-001", "name": "Revenue",              "type": "revenue"}).AssertStatus(http.StatusCreated)
	return ledgerFixture{srv: srv, l: ledger.New()}
}

func (f ledgerFixture) newCtx() *maniflex.ServerContext {
	return maniflex.NewBackground(
		context.Background(),
		f.srv.ManiflexServer().DB(),
		f.srv.ManiflexServer().Registry(),
	)
}

var ledgerToday = time.Now().UTC().Truncate(24 * time.Hour)

// ── Model registration ────────────────────────────────────────────────────────

func TestLedger_ModelsRegisteredAsREST(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	f.srv.GET("/ledger_accounts").AssertStatus(http.StatusOK)
	f.srv.GET("/ledger_entries").AssertStatus(http.StatusOK)
	f.srv.GET("/ledger_lines").AssertStatus(http.StatusOK)
}

// ── Happy path ────────────────────────────────────────────────────────────────

func TestLedger_PostCreatesEntryAndLines(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)
	ctx := f.newCtx()

	entry, err := f.l.Post(ctx,
		ledger.EntryInput{Description: "Invoice #1001", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(10000, "SAR")},
			{AccountID: "REV-001", Credit: money.New(10000, "SAR")},
		},
	)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if entry.ID == "" {
		t.Error("posted entry has no ID")
	}
	if entry.PostedAt == nil {
		t.Error("PostedAt not set")
	}

	resp := f.srv.GET("/ledger_lines?filter=entry_id:eq:" + entry.ID)
	resp.AssertStatus(http.StatusOK)
	if len(resp.DataList()) != 2 {
		t.Errorf("expected 2 lines, got %d", len(resp.DataList()))
	}
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestLedger_PostRejectsSingleLine(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	_, err := f.l.Post(f.newCtx(),
		ledger.EntryInput{Description: "x", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001", Debit: money.New(100, "SAR")},
		},
	)
	if err == nil {
		t.Error("expected ErrNoLines, got nil")
	}
}

func TestLedger_PostRejectsUnbalancedEntry(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	_, err := f.l.Post(f.newCtx(),
		ledger.EntryInput{Description: "x", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(10000, "SAR")},
			{AccountID: "REV-001", Credit: money.New(9000, "SAR")},
		},
	)
	if err == nil {
		t.Error("expected ErrImbalanced, got nil")
	}
}

func TestLedger_PostUnbalancedWritesNothingToDB(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	//nolint:errcheck
	f.l.Post(f.newCtx(),
		ledger.EntryInput{Description: "bad", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(100, "SAR")},
			{AccountID: "REV-001", Credit: money.New(50, "SAR")},
		},
	)

	resp := f.srv.GET("/ledger_entries")
	resp.AssertStatus(http.StatusOK)
	if len(resp.DataList()) != 0 {
		t.Errorf("expected 0 entries after failed post, got %d", len(resp.DataList()))
	}
}

// ── Transaction handling ──────────────────────────────────────────────────────

func TestLedger_PostJoinsActiveTxAndRollbackWorks(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)
	ctx := f.newCtx()

	tx, err := ctx.BeginTx(ctx.Ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	ctx.Tx = tx

	_, postErr := f.l.Post(ctx,
		ledger.EntryInput{Description: "joined", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(200, "SAR")},
			{AccountID: "REV-001", Credit: money.New(200, "SAR")},
		},
	)
	if postErr != nil {
		t.Errorf("Post inside tx: %v", postErr)
	}
	// Roll back — entry must disappear.
	tx.Rollback()
	ctx.Tx = nil

	resp := f.srv.GET("/ledger_entries")
	resp.AssertStatus(http.StatusOK)
	if len(resp.DataList()) != 0 {
		t.Errorf("expected 0 entries after tx rollback, got %d", len(resp.DataList()))
	}
}

// ── Balance ───────────────────────────────────────────────────────────────────

func TestLedger_BalanceReflectsPostedEntry(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)
	ctx := f.newCtx()

	if _, err := f.l.Post(ctx,
		ledger.EntryInput{Description: "sale", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(30000, "SAR")},
			{AccountID: "REV-001", Credit: money.New(30000, "SAR")},
		},
	); err != nil {
		t.Fatalf("Post: %v", err)
	}

	bal, err := ledger.Balance(ctx, "AR-001", "SAR", time.Now())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Cents != 30000 {
		t.Errorf("AR-001 balance: got %d cents, want 30000", bal.Cents)
	}
	if bal.Currency != "SAR" {
		t.Errorf("AR-001 currency: got %q, want SAR", bal.Currency)
	}

	revBal, err := ledger.Balance(ctx, "REV-001", "SAR", time.Now())
	if err != nil {
		t.Fatalf("Balance REV: %v", err)
	}
	if revBal.Cents != -30000 {
		t.Errorf("REV-001 balance: got %d cents, want -30000", revBal.Cents)
	}
}

func TestLedger_BalanceZeroForUnknownAccount(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	bal, err := ledger.Balance(f.newCtx(), "no-such-account", "SAR", time.Now())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Cents != 0 {
		t.Errorf("expected 0, got %d", bal.Cents)
	}
}

func TestLedger_BalanceAsOfExcludesFutureEntries(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)
	ctx := f.newCtx()

	yesterday := ledgerToday.AddDate(0, 0, -1)
	tomorrow  := ledgerToday.AddDate(0, 0, 1)

	if _, err := f.l.Post(ctx,
		ledger.EntryInput{Description: "future", PeriodID: "2026-05", Date: tomorrow},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(5000, "SAR")},
			{AccountID: "REV-001", Credit: money.New(5000, "SAR")},
		},
	); err != nil {
		t.Fatalf("Post: %v", err)
	}

	bal, err := ledger.Balance(ctx, "AR-001", "SAR", yesterday)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Cents != 0 {
		t.Errorf("balance as-of yesterday: got %d, want 0", bal.Cents)
	}
}

// ── TrialBalance ──────────────────────────────────────────────────────────────

func TestLedger_TrialBalanceAllAccounts(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)
	ctx := f.newCtx()

	if _, err := f.l.Post(ctx,
		ledger.EntryInput{Description: "tb test", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(10000, "SAR")},
			{AccountID: "REV-001", Credit: money.New(10000, "SAR")},
		},
	); err != nil {
		t.Fatalf("Post: %v", err)
	}

	tb, err := ledger.TrialBalance(ctx, "SAR", time.Now())
	if err != nil {
		t.Fatalf("TrialBalance: %v", err)
	}
	if len(tb) != 2 {
		t.Fatalf("expected 2 accounts in trial balance, got %d", len(tb))
	}
	balMap := make(map[string]int64, len(tb))
	for _, ab := range tb {
		balMap[ab.AccountID] = ab.Balance.Cents
	}
	if balMap["AR-001"] != 10000 {
		t.Errorf("AR-001 trial balance: got %d, want 10000", balMap["AR-001"])
	}
	if balMap["REV-001"] != -10000 {
		t.Errorf("REV-001 trial balance: got %d, want -10000", balMap["REV-001"])
	}
}

// ── Multi-currency ────────────────────────────────────────────────────────────

func TestLedger_PostMultiCurrencyBalanced(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	_, err := f.l.Post(f.newCtx(),
		ledger.EntryInput{Description: "fx entry", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(1000, "SAR")},
			{AccountID: "REV-001", Credit: money.New(1000, "SAR")},
			{AccountID: "AR-001",  Debit:  money.New(500, "USD")},
			{AccountID: "REV-001", Credit: money.New(500, "USD")},
		},
	)
	if err != nil {
		t.Errorf("Post multi-currency: %v", err)
	}
}

func TestLedger_PostMultiCurrencyUnbalancedFails(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	_, err := f.l.Post(f.newCtx(),
		ledger.EntryInput{Description: "bad fx", PeriodID: "2026-05", Date: ledgerToday},
		[]ledger.LineInput{
			{AccountID: "AR-001",  Debit:  money.New(1000, "SAR")},
			{AccountID: "REV-001", Credit: money.New(1000, "SAR")},
			{AccountID: "AR-001",  Debit:  money.New(500, "USD")},
			{AccountID: "REV-001", Credit: money.New(300, "USD")}, // unbalanced USD
		},
	)
	if err == nil {
		t.Error("expected ErrImbalanced for unbalanced USD, got nil")
	}
}

// ── posted_at is read-only via REST ───────────────────────────────────────────

func TestLedger_PostedAtCannotBeSetViaREST(t *testing.T) {
	t.Parallel()
	f := newLedgerFixture(t)

	resp := f.srv.POST("/ledger_entries", map[string]any{
		"description": "rest entry",
		"period_id":   "2026-05",
		"date":        ledgerToday.Format(time.RFC3339),
	})
	resp.AssertStatus(http.StatusCreated)
	data := resp.Data()

	if data["posted_at"] != nil && data["posted_at"] != "" {
		t.Errorf("expected posted_at to be empty on REST-created entry, got %v", data["posted_at"])
	}
}
