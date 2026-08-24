package maniflex

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// lock_when compared fmt.Sprintf("%v", value) against the tag text. %v renders a
// float64 the way %g does, so a field holding 1000000 stringified to "1e+06"
// and a tag saying 1000000 never matched — the lock silently did not fire
// (audit O8). Failing open is the wrong direction for a guard whose whole
// purpose is to refuse writes to a finalised record.
type lockFloatModel struct {
	BaseModel
	Note string  `json:"note"`
	Big  float64 `json:"big" mfx:"lock_when:big=1000000"`
}

// The value side of the directive was never checked against the field it
// compares. Only the field NAME was resolved, so a tag whose value the field can
// never hold registered happily and produced a rule that could not fire.
type lockWordForIntModel struct {
	BaseModel
	N int `json:"n" mfx:"lock_when:n=five"`
}

type lockBoolAsOneModel struct {
	BaseModel
	Flag bool `json:"flag" mfx:"lock_when:flag=1"`
}

type lockUnsupportedTypeModel struct {
	BaseModel
	When time.Time `json:"when" mfx:"lock_when:when=2026-01-01"`
}

type lockPtrModel struct {
	BaseModel
	Flag *bool `json:"flag" mfx:"lock_when:flag=true"`
}

type lockOKModel struct {
	BaseModel
	Status string  `json:"status" mfx:"lock_when:status=posted"`
	Flag   bool    `json:"flag"   mfx:"lock_when:flag=true"`
	N      int     `json:"n"      mfx:"lock_when:n=5"`
	Ratio  float64 `json:"ratio"  mfx:"lock_when:ratio=1.5"`
}

func registerLock(t *testing.T, model any) error {
	t.Helper()
	return New(Config{DisableAutoMigrate: true}).Register(model)
}

func TestLockWhenValidatesValueAgainstFieldType(t *testing.T) {
	t.Run("a value the field can never hold is refused", func(t *testing.T) {
		err := registerLock(t, lockWordForIntModel{})
		if err == nil {
			t.Fatal("registered lock_when:n=five on an int field")
		}
		for _, want := range []string{"n", "five"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("an unsupported field type is refused", func(t *testing.T) {
		err := registerLock(t, lockUnsupportedTypeModel{})
		if err == nil {
			t.Fatal("registered lock_when against a time.Time field")
		}
		// Asserted on the wording, not merely on err != nil: without the type
		// check the value parse rejects it anyway, for a reason that has nothing
		// to do with the real problem. "2026-01-01 is not a valid time.Time"
		// sends the reader off to fix the date.
		if !strings.Contains(err.Error(), "supports string, bool and numeric") {
			t.Errorf("error blames the value rather than the field's type: %v", err)
		}
	})

	t.Run("1 is accepted for a bool", func(t *testing.T) {
		// strconv.ParseBool takes it, and the author plainly meant true. Being
		// forgiving here costs nothing; the comparison is typed either way.
		if err := registerLock(t, lockBoolAsOneModel{}); err != nil {
			t.Fatalf("rejected lock_when:flag=1 on a bool field: %v", err)
		}
	})

	t.Run("ordinary directives still register", func(t *testing.T) {
		if err := registerLock(t, lockOKModel{}); err != nil {
			t.Fatalf("rejected a valid set of directives: %v", err)
		}
	})

	t.Run("a float bound registers", func(t *testing.T) {
		if err := registerLock(t, lockFloatModel{}); err != nil {
			t.Fatalf("rejected lock_when:big=1000000 on a float field: %v", err)
		}
	})

	// A nullable column is a pointer field, and locking on one is ordinary —
	// "finalised" is exactly the kind of flag that starts out unset.
	t.Run("a pointer field is resolved through its element type", func(t *testing.T) {
		meta, err := buildLockMeta(t, lockPtrModel{})
		if err != nil {
			t.Fatalf("rejected lock_when on a *bool field: %v", err)
		}
		if len(meta.LockWhen) != 1 {
			t.Fatalf("collected %d conditions, want 1", len(meta.LockWhen))
		}
		if !meta.LockWhen[0].matchesRecord(map[string]any{"flag": true}) {
			t.Error("a *bool field holding true did not match lock_when:flag=true")
		}
	})
}

// The comparison itself, across the shapes a driver hands back for one column:
// SQLite reports a bool column as int64, Postgres as bool, and a numeric one can
// arrive as int64, float64 or — for Postgres NUMERIC through lib/pq — a string.
// Every one of them has to reach the same verdict.
func TestLockConditionMatchesAcrossDriverTypes(t *testing.T) {
	meta, err := buildLockMeta(t, lockOKModel{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	byField := map[string]LockCondition{}
	for _, lc := range meta.LockWhen {
		byField[lc.JSONName] = lc
	}

	cases := []struct {
		field string
		value any
		want  bool
	}{
		{"status", "posted", true},
		{"status", "draft", false},
		{"flag", true, true},
		{"flag", int64(1), true}, // SQLite has no bool type
		{"flag", false, false},
		{"flag", int64(0), false},
		{"n", 5, true},
		{"n", int64(5), true},
		{"n", float64(5), true}, // a JSON round-trip widens it
		{"n", "5", true},        // lib/pq hands NUMERIC back as text
		{"n", 6, false},
		{"ratio", 1.5, true},
		{"ratio", "1.5", true},
		{"ratio", 2.0, false},
	}
	for _, tc := range cases {
		lc, ok := byField[tc.field]
		if !ok {
			t.Fatalf("no condition collected for %q", tc.field)
		}
		got := lc.matchesRecord(map[string]any{tc.field: tc.value})
		if got != tc.want {
			t.Errorf("%s = %#v (%T): matched=%v, want %v", tc.field, tc.value, tc.value, got, tc.want)
		}
	}
}

// A float that %v would render in exponent form is the case that started this.
func TestLockConditionMatchesLargeFloat(t *testing.T) {
	meta, err := buildLockMeta(t, lockFloatModel{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(meta.LockWhen) != 1 {
		t.Fatalf("collected %d conditions, want 1", len(meta.LockWhen))
	}
	lc := meta.LockWhen[0]
	if !lc.matchesRecord(map[string]any{"big": float64(1000000)}) {
		t.Error("1000000 in a float column did not match a lock_when saying 1000000")
	}
	if lc.matchesRecord(map[string]any{"big": float64(999999)}) {
		t.Error("999999 matched a lock_when saying 1000000")
	}
}

func buildLockMeta(t *testing.T, model any) (*ModelMeta, error) {
	t.Helper()
	s := New(Config{DisableAutoMigrate: true})
	if err := s.Register(model); err != nil {
		return nil, err
	}
	name := reflect.TypeOf(model).Name()
	meta, ok := s.Registry().Get(name)
	if !ok {
		t.Fatalf("model %q not in the registry after Register", name)
	}
	return meta, nil
}
