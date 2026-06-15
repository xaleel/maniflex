package maniflex_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"maniflex"
)

// captureWarnings installs a slog handler that records WARN+ output into a
// buffer for the duration of the test, restoring the previous default after.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// scan is a helper that scans a model and fails on a hard error.
func scan(t *testing.T, v any) *maniflex.ModelMeta {
	t.Helper()
	meta, err := maniflex.ScanModel(v, maniflex.ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel(%T): %v", v, err)
	}
	return meta
}

// ── Tag-parsing tests ─────────────────────────────────────────────────────────

type schedTagModel struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Status     string     `json:"status"      mfx:"enum:draft|published,filterable"`
	Color      string     `json:"color"`
	Bare       *time.Time `json:"bare"        mfx:"scheduled"`
	Soft       *time.Time `json:"soft"        mfx:"scheduled;soft-delete"`
	Hard       *time.Time `json:"hard"        mfx:"scheduled;hard-delete"`
	Publish    *time.Time `json:"publish"     mfx:"scheduled;field=status;from=draft;to=published"`
	Recolor    *time.Time `json:"recolor"     mfx:"scheduled;field=color;to=red"`
	Bogus      *time.Time `json:"bogus"       mfx:"scheduled;bogus"`
	WithFilter *time.Time `json:"with_filter" mfx:"scheduled;soft-delete,filterable"`
}

func TestScheduledTag_Parsing(t *testing.T) {
	captureWarnings(t)
	meta := scan(t, schedTagModel{})

	tag := func(db string) maniflex.FieldTags {
		f := meta.FieldByDBName(db)
		if f == nil {
			t.Fatalf("field %q not found", db)
		}
		return f.Tags
	}

	t.Run("bare scheduled records only the switch", func(t *testing.T) {
		tg := tag("bare")
		if !tg.Scheduled {
			t.Error("Scheduled should be true")
		}
		if tg.SchedSoft || tg.SchedHard || tg.SchedField != "" || tg.SchedHasTo || tg.SchedHasFrom {
			t.Errorf("bare tag should record no action: %+v", tg)
		}
	})

	t.Run("soft-delete", func(t *testing.T) {
		if tg := tag("soft"); !tg.Scheduled || !tg.SchedSoft {
			t.Errorf("want Scheduled+SchedSoft, got %+v", tg)
		}
	})

	t.Run("hard-delete", func(t *testing.T) {
		if tg := tag("hard"); !tg.Scheduled || !tg.SchedHard {
			t.Errorf("want Scheduled+SchedHard, got %+v", tg)
		}
	})

	t.Run("field with from and to", func(t *testing.T) {
		tg := tag("publish")
		if tg.SchedField != "status" || tg.SchedFrom != "draft" || tg.SchedTo != "published" {
			t.Errorf("field/from/to mismatch: %+v", tg)
		}
		if !tg.SchedHasFrom || !tg.SchedHasTo {
			t.Errorf("HasFrom/HasTo should both be true: %+v", tg)
		}
	})

	t.Run("field with to only", func(t *testing.T) {
		tg := tag("recolor")
		if tg.SchedField != "color" || tg.SchedTo != "red" {
			t.Errorf("field/to mismatch: %+v", tg)
		}
		if tg.SchedHasFrom {
			t.Errorf("HasFrom should be false: %+v", tg)
		}
		if !tg.SchedHasTo {
			t.Errorf("HasTo should be true: %+v", tg)
		}
	})

	t.Run("unrecognised option recorded in SchedBadOpt", func(t *testing.T) {
		if tg := tag("bogus"); tg.SchedBadOpt != "bogus" {
			t.Errorf("SchedBadOpt = %q, want \"bogus\"", tg.SchedBadOpt)
		}
	})

	t.Run("trailing comma-part still parsed", func(t *testing.T) {
		tg := tag("with_filter")
		if !tg.SchedSoft {
			t.Errorf("scheduled directive lost: %+v", tg)
		}
		if !tg.Filterable {
			t.Errorf("trailing filterable comma-part lost: %+v", tg)
		}
	})
}

// ── Registration-validation tests (non-strict mode) ───────────────────────────

// scheduledColumns returns the set of driving columns in meta.Scheduled().
func scheduledColumns(meta *maniflex.ModelMeta) map[string]bool {
	out := map[string]bool{}
	for _, s := range meta.Scheduled() {
		out[s.Column] = true
	}
	return out
}

type validSchedModel struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Status    string     `json:"status"  mfx:"enum:draft|published"`
	Publish   *time.Time `json:"publish"`
	Expires   *time.Time `json:"expires" mfx:"scheduled;soft-delete"`
	Purge     *time.Time `json:"purge"   mfx:"scheduled;hard-delete"`
	PublishAt *time.Time `json:"publish_at" mfx:"scheduled;field=status;from=draft;to=published"`
}

func TestScheduled_ValidModel(t *testing.T) {
	captureWarnings(t)
	meta := scan(t, validSchedModel{})

	if !meta.HasScheduled() {
		t.Fatal("HasScheduled should be true")
	}
	cols := scheduledColumns(meta)
	for _, want := range []string{"expires", "purge", "publish_at"} {
		if !cols[want] {
			t.Errorf("spec for column %q missing; got %v", want, cols)
		}
	}
	if len(meta.Scheduled()) != 3 {
		t.Fatalf("want 3 specs, got %d", len(meta.Scheduled()))
	}
}

type noSchedModel struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

func TestScheduled_NoTagModel(t *testing.T) {
	captureWarnings(t)
	meta := scan(t, noSchedModel{})
	if meta.HasScheduled() {
		t.Error("HasScheduled should be false")
	}
	if len(meta.Scheduled()) != 0 {
		t.Errorf("Scheduled() should be empty, got %v", meta.Scheduled())
	}
}

type bannerModel struct {
	maniflex.BaseModel
	Color        string     `json:"color"`
	HolidayStart *time.Time `json:"holiday_start" mfx:"scheduled;field=color;to=red"`
	HolidayEnd   *time.Time `json:"holiday_end"   mfx:"scheduled;field=color;from=red;to=blue"`
}

func TestScheduled_TwoFieldsBothPresent(t *testing.T) {
	captureWarnings(t)
	meta := scan(t, bannerModel{})
	if len(meta.Scheduled()) != 2 {
		t.Fatalf("want 2 specs, got %d", len(meta.Scheduled()))
	}
}

// invalidCase asserts that scanning v drops the field with driving column
// `dropped`, logs a warning, and leaves the rest of the model intact.
func assertDropped(t *testing.T, v any, dropped string, warnSubstr string) {
	t.Helper()
	buf := captureWarnings(t)
	meta := scan(t, v)
	if scheduledColumns(meta)[dropped] {
		t.Errorf("column %q should have been dropped from Scheduled(); got %v",
			dropped, scheduledColumns(meta))
	}
	logs := buf.String()
	if !strings.Contains(logs, "invalid scheduled tag") {
		t.Errorf("expected an invalid-scheduled-tag warning, logs:\n%s", logs)
	}
	if warnSubstr != "" && !strings.Contains(logs, warnSubstr) {
		t.Errorf("warning should mention %q, logs:\n%s", warnSubstr, logs)
	}
}

type bareActionModel struct {
	maniflex.BaseModel
	When *time.Time `json:"when" mfx:"scheduled"`
}

func TestScheduled_Invalid_BareNoAction(t *testing.T) {
	assertDropped(t, bareActionModel{}, "when", "no action")
}

type bogusOptModel struct {
	maniflex.BaseModel
	When *time.Time `json:"when" mfx:"scheduled;bogus"`
}

func TestScheduled_Invalid_UnknownOption(t *testing.T) {
	assertDropped(t, bogusOptModel{}, "when", "bogus")
}

type nonTimeModel struct {
	maniflex.BaseModel
	When string `json:"when" mfx:"scheduled;soft-delete"`
}

func TestScheduled_Invalid_NonTimeField(t *testing.T) {
	assertDropped(t, nonTimeModel{}, "when", "*time.Time")
}

type nonPtrTimeModel struct {
	maniflex.BaseModel
	When time.Time `json:"when" mfx:"scheduled;hard-delete"`
}

func TestScheduled_Invalid_NonPointerTime(t *testing.T) {
	assertDropped(t, nonPtrTimeModel{}, "when", "*time.Time")
}

type conflictDeleteModel struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	When *time.Time `json:"when" mfx:"scheduled;soft-delete;hard-delete"`
}

func TestScheduled_Invalid_ConflictingDeletes(t *testing.T) {
	assertDropped(t, conflictDeleteModel{}, "when", "conflicting")
}

type conflictHardFieldModel struct {
	maniflex.BaseModel
	Status string     `json:"status"`
	When   *time.Time `json:"when" mfx:"scheduled;hard-delete;field=status;to=done"`
}

func TestScheduled_Invalid_ConflictingHardAndField(t *testing.T) {
	assertDropped(t, conflictHardFieldModel{}, "when", "conflicting")
}

type fieldNoToModel struct {
	maniflex.BaseModel
	Status string     `json:"status"`
	When   *time.Time `json:"when" mfx:"scheduled;field=status"`
}

func TestScheduled_Invalid_FieldWithoutTo(t *testing.T) {
	assertDropped(t, fieldNoToModel{}, "when", "to=")
}

type toNoFieldModel struct {
	maniflex.BaseModel
	When *time.Time `json:"when" mfx:"scheduled;to=v"`
}

func TestScheduled_Invalid_ToWithoutField(t *testing.T) {
	// `to=` alone declares no action either — both messages are acceptable;
	// the field must be dropped.
	assertDropped(t, toNoFieldModel{}, "when", "")
}

type softNotDeletableModel struct {
	maniflex.BaseModel
	When *time.Time `json:"when" mfx:"scheduled;soft-delete"`
}

func TestScheduled_Invalid_SoftDeleteNonDeletable(t *testing.T) {
	assertDropped(t, softNotDeletableModel{}, "when", "WithDeletedAt")
}

type hardNotDeletableModel struct {
	maniflex.BaseModel
	When *time.Time `json:"when" mfx:"scheduled;hard-delete"`
}

func TestScheduled_Valid_HardDeleteNonDeletable(t *testing.T) {
	buf := captureWarnings(t)
	meta := scan(t, hardNotDeletableModel{})
	if !scheduledColumns(meta)["when"] {
		t.Error("hard-delete on a non-soft-deletable model should be valid")
	}
	if strings.Contains(buf.String(), "invalid scheduled tag") {
		t.Errorf("no warning expected, logs:\n%s", buf.String())
	}
}

type missingColModel struct {
	maniflex.BaseModel
	When *time.Time `json:"when" mfx:"scheduled;field=nonexistent;to=v"`
}

func TestScheduled_Invalid_MissingTargetColumn(t *testing.T) {
	assertDropped(t, missingColModel{}, "when", "does not exist")
}

type enumMismatchModel struct {
	maniflex.BaseModel
	Status string     `json:"status" mfx:"enum:draft|published"`
	When   *time.Time `json:"when"   mfx:"scheduled;field=status;to=archived"`
}

func TestScheduled_Invalid_EnumMismatchTo(t *testing.T) {
	assertDropped(t, enumMismatchModel{}, "when", "enum")
}

type enumMismatchFromModel struct {
	maniflex.BaseModel
	Status string     `json:"status" mfx:"enum:draft|published"`
	When   *time.Time `json:"when"   mfx:"scheduled;field=status;from=archived;to=published"`
}

func TestScheduled_Invalid_EnumMismatchFrom(t *testing.T) {
	assertDropped(t, enumMismatchFromModel{}, "when", "enum")
}

type mixedValidInvalidModel struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Status  string     `json:"status"`
	Expires *time.Time `json:"expires" mfx:"scheduled;soft-delete"`        // valid
	Broken  *time.Time `json:"broken"  mfx:"scheduled;field=nope;to=v"`    // invalid
}

func TestScheduled_MixedValidAndInvalid(t *testing.T) {
	captureWarnings(t)
	meta := scan(t, mixedValidInvalidModel{})
	cols := scheduledColumns(meta)
	if !cols["expires"] {
		t.Error("valid field 'expires' should survive")
	}
	if cols["broken"] {
		t.Error("invalid field 'broken' should be dropped")
	}
	if len(meta.Scheduled()) != 1 {
		t.Fatalf("want 1 surviving spec, got %d", len(meta.Scheduled()))
	}
}

// ── Auto-index tests ──────────────────────────────────────────────────────────

func hasIndexOnColumn(meta *maniflex.ModelMeta, col string) int {
	count := 0
	for _, idx := range meta.Indices {
		if len(idx.Columns) == 1 && idx.Columns[0] == col {
			count++
		}
	}
	return count
}

func TestScheduled_AutoIndex_Appended(t *testing.T) {
	captureWarnings(t)
	meta := scan(t, validSchedModel{})
	for _, col := range []string{"expires", "purge", "publish_at"} {
		if n := hasIndexOnColumn(meta, col); n != 1 {
			t.Errorf("expected exactly one auto-index on %q, got %d", col, n)
		}
	}
}

type userIndexedSchedModel struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Expires *time.Time `json:"expires" mfx:"scheduled;soft-delete"`
}

func TestScheduled_AutoIndex_NotDuplicatedWhenUserDeclared(t *testing.T) {
	captureWarnings(t)
	meta, err := maniflex.ScanModel(userIndexedSchedModel{}, maniflex.ModelConfig{
		Indices: []maniflex.IndexSpec{{Name: "my_idx", Columns: []string{"expires"}}},
	})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	if n := hasIndexOnColumn(meta, "expires"); n != 1 {
		t.Errorf("expected the single user-declared index on 'expires', got %d", n)
	}
}
