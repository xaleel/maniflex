package e2e

// Presence semantics on the typed write path. The map→struct migration's single
// biggest risk is conflating a field that is ABSENT from the request with one
// that is PRESENT but holds its Go zero value (0 / false / ""). Presence must be
// captured from the request's top-level keys, never inferred from whether the
// struct field equals its zero value — otherwise PATCH/POST silently lose data
// or clobber columns with zeros.
//
// presenceGadget gives every scalar a SQL DEFAULT that DIFFERS from its Go zero
// value, so "omitted → DB default" is observably distinct from "explicit zero →
// stored zero". Bio is a nullable pointer with no default, for the PATCH-null
// three-way (set / clear / leave-unchanged).

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type presenceGadget struct {
	maniflex.BaseModel
	Name   string  `json:"name"   db:"name"   mfx:"required"`
	Count  int     `json:"count"  db:"count"  mfx:"default:7"`
	Active bool    `json:"active" db:"active" mfx:"default:true"`
	Label  string  `json:"label"  db:"label"  mfx:"default:hello"`
	Bio    *string `json:"bio"    db:"bio"`
}

// P0 #1 — an explicit zero value is PRESENT and must persist, overriding the
// column default; an omitted field is ABSENT and must take the column default.
func TestPresence_ZeroValueVsAbsentOnCreate(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{presenceGadget{}}})

	// Explicit zeros — these keys are present in the body, so 0/false/"" must
	// be written, NOT the defaults 7/true/"hello".
	zero := srv.POST("/presence_gadgets", map[string]any{
		"name":   "explicit",
		"count":  0,
		"active": false,
		"label":  "",
	})
	zero.AssertStatus(http.StatusCreated)
	// Read back via a fresh GET (authoritative DB state via scanStruct), since the
	// create response need not reflect DB-applied defaults.
	got := srv.GET("/presence_gadgets/" + zero.ID()).Data()
	if v := testutil.FloatField(t, got, "count"); v != 0 {
		t.Errorf("explicit count: got %v, want 0 (present zero must persist, not default 7)", v)
	}
	if v := testutil.BoolField(t, got, "active"); v != false {
		t.Errorf("explicit active: got %v, want false (present zero must persist, not default true)", v)
	}
	if v := testutil.Field(t, got, "label"); v != "" {
		t.Errorf("explicit label: got %q, want empty (present zero must persist, not default hello)", v)
	}

	// Omitted fields — absent from the body, so the DB defaults apply.
	def := srv.POST("/presence_gadgets", map[string]any{"name": "defaulted"})
	def.AssertStatus(http.StatusCreated)
	got2 := srv.GET("/presence_gadgets/" + def.ID()).Data()
	if v := testutil.FloatField(t, got2, "count"); v != 7 {
		t.Errorf("absent count: got %v, want 7 (column default)", v)
	}
	if v := testutil.BoolField(t, got2, "active"); v != true {
		t.Errorf("absent active: got %v, want true (column default)", v)
	}
	if v := testutil.Field(t, got2, "label"); v != "hello" {
		t.Errorf("absent label: got %q, want hello (column default)", v)
	}
}

// P0 #2 — PATCH three-way presence on a nullable column: explicit null clears it
// to NULL, an explicit value sets it, and omitting it leaves the column
// unchanged. Only the null-clears half was previously untested.
func TestPresence_PatchNullClearsNullableField(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{presenceGadget{}}})

	created := srv.POST("/presence_gadgets", map[string]any{"name": "p", "bio": "first bio"})
	created.AssertStatus(http.StatusCreated)
	id := created.ID()
	if v := srv.GET("/presence_gadgets/" + id).Data()["bio"]; v != "first bio" {
		t.Fatalf("seed bio = %v, want %q", v, "first bio")
	}

	// Explicit null → column cleared to NULL.
	srv.PATCH("/presence_gadgets/"+id, map[string]any{"bio": nil}).AssertStatus(http.StatusOK)
	if v := srv.GET("/presence_gadgets/" + id).Data()["bio"]; v != nil {
		t.Errorf("after PATCH {bio:null}: bio = %v, want nil (null must clear the column)", v)
	}

	// Explicit value → set.
	srv.PATCH("/presence_gadgets/"+id, map[string]any{"bio": "second"}).AssertStatus(http.StatusOK)
	if v := srv.GET("/presence_gadgets/" + id).Data()["bio"]; v != "second" {
		t.Errorf("after PATCH {bio:\"second\"}: bio = %v, want %q", v, "second")
	}

	// Omitted → unchanged (patch a different field).
	srv.PATCH("/presence_gadgets/"+id, map[string]any{"name": "renamed"}).AssertStatus(http.StatusOK)
	if v := srv.GET("/presence_gadgets/" + id).Data()["bio"]; v != "second" {
		t.Errorf("after PATCH omitting bio: bio = %v, want %q (omitted must leave unchanged)", v, "second")
	}
}
