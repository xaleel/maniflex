package ledger

import (
	"errors"
	"testing"

	"github.com/xaleel/maniflex/pkg/money"
)

// ── validateLines ─────────────────────────────────────────────────────────────

func TestValidateLines_MissingAccountID(t *testing.T) {
	lines := []LineInput{{Debit: money.New(100, "SAR")}} // no AccountID
	if err := validateLines(lines); err == nil {
		t.Fatal("expected error for missing account_id, got nil")
	}
}

func TestValidateLines_NegativeDebit(t *testing.T) {
	lines := []LineInput{{AccountID: "a", Debit: money.New(-1, "SAR")}}
	if err := validateLines(lines); err == nil {
		t.Fatal("expected error for negative debit, got nil")
	}
}

func TestValidateLines_NegativeCredit(t *testing.T) {
	lines := []LineInput{{AccountID: "a", Credit: money.New(-1, "SAR")}}
	if err := validateLines(lines); err == nil {
		t.Fatal("expected error for negative credit, got nil")
	}
}

func TestValidateLines_BothDebitAndCredit(t *testing.T) {
	lines := []LineInput{{
		AccountID: "a",
		Debit:     money.New(100, "SAR"),
		Credit:    money.New(100, "SAR"),
	}}
	if err := validateLines(lines); err == nil {
		t.Fatal("expected error when both debit and credit are non-zero, got nil")
	}
}

func TestValidateLines_BothZero(t *testing.T) {
	lines := []LineInput{{AccountID: "a"}} // both Debit and Credit are zero-value
	if err := validateLines(lines); err == nil {
		t.Fatal("expected error when both debit and credit are zero, got nil")
	}
}

func TestValidateLines_MissingCurrency(t *testing.T) {
	// Debit amount is non-zero but currency is empty.
	lines := []LineInput{{AccountID: "a", Debit: money.Amount{Cents: 100}}}
	if err := validateLines(lines); err == nil {
		t.Fatal("expected error for missing currency, got nil")
	}
}

func TestValidateLines_Valid(t *testing.T) {
	lines := []LineInput{
		{AccountID: "a", Debit: money.New(100, "SAR")},
		{AccountID: "b", Credit: money.New(100, "SAR")},
	}
	if err := validateLines(lines); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ── validateBalance ───────────────────────────────────────────────────────────

func TestValidateBalance_Balanced(t *testing.T) {
	lines := []LineInput{
		{AccountID: "ar",  Debit:  money.New(500, "SAR")},
		{AccountID: "rev", Credit: money.New(500, "SAR")},
	}
	if err := validateBalance(lines); err != nil {
		t.Fatalf("expected nil for balanced entry, got %v", err)
	}
}

func TestValidateBalance_Unbalanced(t *testing.T) {
	lines := []LineInput{
		{AccountID: "ar",  Debit:  money.New(500, "SAR")},
		{AccountID: "rev", Credit: money.New(400, "SAR")}, // 100 short
	}
	if err := validateBalance(lines); err == nil {
		t.Fatal("expected ErrImbalanced, got nil")
	} else if !errors.Is(err, ErrImbalanced) {
		t.Fatalf("expected ErrImbalanced, got %v", err)
	}
}

func TestValidateBalance_MultiCurrencyBalanced(t *testing.T) {
	lines := []LineInput{
		{AccountID: "ar-sar",  Debit:  money.New(1000, "SAR")},
		{AccountID: "rev-sar", Credit: money.New(1000, "SAR")},
		{AccountID: "ar-usd",  Debit:  money.New(500, "USD")},
		{AccountID: "rev-usd", Credit: money.New(500, "USD")},
	}
	if err := validateBalance(lines); err != nil {
		t.Fatalf("expected nil for multi-currency balanced entry, got %v", err)
	}
}

func TestValidateBalance_MultiCurrencyUnbalanced(t *testing.T) {
	lines := []LineInput{
		{AccountID: "ar-sar",  Debit:  money.New(1000, "SAR")},
		{AccountID: "rev-sar", Credit: money.New(1000, "SAR")},
		{AccountID: "ar-usd",  Debit:  money.New(500, "USD")},
		{AccountID: "rev-usd", Credit: money.New(300, "USD")}, // unbalanced USD
	}
	err := validateBalance(lines)
	if err == nil {
		t.Fatal("expected ErrImbalanced for unbalanced USD leg, got nil")
	}
	if !errors.Is(err, ErrImbalanced) {
		t.Fatalf("expected ErrImbalanced, got %v", err)
	}
}

func TestValidateBalance_CreditOnlyLeg(t *testing.T) {
	// credits with no matching debit in the debits map
	lines := []LineInput{
		{AccountID: "a", Credit: money.New(100, "SAR")},
		// No debit for SAR at all — only credits key exists
	}
	// debits["SAR"] == 0, credits["SAR"] == 100 → mismatch
	if err := validateBalance(lines); err == nil {
		t.Fatal("expected ErrImbalanced when only credits present, got nil")
	}
}

// ── lineCurrency ──────────────────────────────────────────────────────────────

func TestLineCurrency_PreferDebit(t *testing.T) {
	l := LineInput{
		AccountID: "a",
		Debit:     money.New(100, "SAR"),
	}
	if got := lineCurrency(l); got != "SAR" {
		t.Errorf("lineCurrency: got %q, want SAR", got)
	}
}

func TestLineCurrency_FallsBackToCredit(t *testing.T) {
	l := LineInput{
		AccountID: "b",
		Credit:    money.New(100, "USD"),
	}
	if got := lineCurrency(l); got != "USD" {
		t.Errorf("lineCurrency: got %q, want USD", got)
	}
}

// ── toInt64 ───────────────────────────────────────────────────────────────────

func TestToInt64(t *testing.T) {
	cases := []struct {
		input any
		want  int64
	}{
		{int64(42), 42},
		{int(42), 42},
		{float64(42.9), 42},
		{[]byte("123"), 123},
		{nil, 0},
	}
	for _, c := range cases {
		if got := toInt64(c.input); got != c.want {
			t.Errorf("toInt64(%T=%v): got %d, want %d", c.input, c.input, got, c.want)
		}
	}
}
