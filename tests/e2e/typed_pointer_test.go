package e2e

// P2 #12 — pointer scalar fields round-trip through the typed read/write path.
// This guards the normalise/normaliseTx pointer-deref fix (the ledger work):
// a nil pointer must store/read as NULL, a set pointer must store/read its value,
// and a pointer-to-zero must be distinct from nil (0 persists, not NULL).

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type ptrThing struct {
	maniflex.BaseModel
	Name string  `json:"name" db:"name" mfx:"required"`
	Num  *int    `json:"num"  db:"num"`
	Text *string `json:"text" db:"text"`
}

func TestPointerFields_NilAndSetRoundTrip(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{ptrThing{}}})

	// Set values round-trip.
	withVals := srv.POST("/ptr_things", map[string]any{"name": "v", "num": 42, "text": "hi"})
	withVals.AssertStatus(http.StatusCreated)
	got := srv.GET("/ptr_things/" + withVals.ID()).Data()
	if v := testutil.FloatField(t, got, "num"); v != 42 {
		t.Errorf("num = %v, want 42", v)
	}
	if v := testutil.Field(t, got, "text"); v != "hi" {
		t.Errorf("text = %q, want hi", v)
	}

	// Omitted pointers → NULL → null in the response.
	nils := srv.POST("/ptr_things", map[string]any{"name": "n"})
	nils.AssertStatus(http.StatusCreated)
	gotNil := srv.GET("/ptr_things/" + nils.ID()).Data()
	if gotNil["num"] != nil {
		t.Errorf("omitted num = %v, want nil", gotNil["num"])
	}
	if gotNil["text"] != nil {
		t.Errorf("omitted text = %v, want nil", gotNil["text"])
	}

	// Pointer-to-zero is distinct from nil: a present num:0 persists as 0, not NULL.
	zero := srv.POST("/ptr_things", map[string]any{"name": "z", "num": 0})
	zero.AssertStatus(http.StatusCreated)
	gotZero := srv.GET("/ptr_things/" + zero.ID()).Data()
	if gotZero["num"] == nil {
		t.Error("present num:0 should persist as 0, not NULL (pointer-to-zero vs nil)")
	} else if v := testutil.FloatField(t, gotZero, "num"); v != 0 {
		t.Errorf("present num = %v, want 0", v)
	}
}
