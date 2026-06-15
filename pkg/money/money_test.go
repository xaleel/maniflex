package money_test

import (
	"encoding/json"
	"testing"

	"maniflex"
	"maniflex/pkg/money"
)

func TestSQLType(t *testing.T) {
	a := money.Amount{}
	if got := a.SQLType(maniflex.Postgres); got != "NUMERIC(19,4)" {
		t.Errorf("Postgres: got %q, want NUMERIC(19,4)", got)
	}
	if got := a.SQLType(maniflex.SQLite); got != "TEXT" {
		t.Errorf("SQLite: got %q, want TEXT", got)
	}
}

func TestDriverValue(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{1234, "12.3400"},
		{0, "0.0000"},
		{100, "1.0000"},
		{5, "0.0500"},
		{-1234, "-12.3400"},
	}
	for _, c := range cases {
		a := money.New(c.cents, "SAR")
		v, err := a.Value()
		if err != nil {
			t.Fatalf("cents=%d Value(): %v", c.cents, err)
		}
		if got, ok := v.(string); !ok || got != c.want {
			t.Errorf("cents=%d: got %q, want %q", c.cents, v, c.want)
		}
	}
}

func TestScan(t *testing.T) {
	cases := []struct {
		src       any
		wantCents int64
	}{
		{"12.3400", 1234},
		{"0.0000", 0},
		{"-12.3400", -1234},
		{"12.34", 1234},
		{"12.3", 1230},
		{float64(12.34), 1234},
		{int64(12), 1200},
		{nil, 0},
		{[]byte("12.3400"), 1234},
	}
	for _, c := range cases {
		var a money.Amount
		if err := a.Scan(c.src); err != nil {
			t.Fatalf("Scan(%v): %v", c.src, err)
		}
		if a.Cents != c.wantCents {
			t.Errorf("Scan(%v): got Cents=%d, want %d", c.src, a.Cents, c.wantCents)
		}
	}
}

func TestJSON(t *testing.T) {
	orig := money.New(1234, "SAR")
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"amount":1234,"currency":"SAR"}`
	if string(b) != want {
		t.Errorf("Marshal: got %s, want %s", b, want)
	}

	var decoded money.Amount
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Cents != orig.Cents || decoded.Currency != orig.Currency {
		t.Errorf("round-trip: got %+v, want %+v", decoded, orig)
	}
}

func TestAdd(t *testing.T) {
	a := money.New(1000, "SAR")
	b := money.New(234, "SAR")
	got, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got.Cents != 1234 || got.Currency != "SAR" {
		t.Errorf("Add: got %+v, want {1234 SAR}", got)
	}
}

func TestAddCurrencyMismatch(t *testing.T) {
	_, err := money.New(100, "SAR").Add(money.New(100, "USD"))
	if err == nil {
		t.Fatal("expected ErrCurrencyMismatch, got nil")
	}
}

func TestSub(t *testing.T) {
	got, err := money.New(1234, "SAR").Sub(money.New(234, "SAR"))
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	if got.Cents != 1000 {
		t.Errorf("Sub: got %d, want 1000", got.Cents)
	}
}

func TestMul(t *testing.T) {
	a := money.New(1000, "SAR")
	// 1000 cents × 3/2 = 1500 cents
	got := a.Mul(3, 2)
	if got.Cents != 1500 || got.Currency != "SAR" {
		t.Errorf("Mul: got %+v, want {1500 SAR}", got)
	}
}

func TestImplementsSQLTyper(t *testing.T) {
	var _ maniflex.SQLTyper = money.Amount{}
}
