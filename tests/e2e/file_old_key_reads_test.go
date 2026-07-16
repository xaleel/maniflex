package e2e

// Replacing an mfx:"auto_delete" file needs the key it is replacing, so the file
// step reads the pre-write record — but it asked once per file field, re-reading
// the same row and rebuilding the same column map for each one. A model with three
// auto_delete fields read the same row three times to pull three values out of it
// (PERF-4). The read is now memoised for the request.
//
// This counts the adapter calls rather than trusting the shape of the code: the
// point of the change is the number of round-trips.

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// multiFileDoc has three auto_delete file fields (the default for mfx:"file"), so
// one update replacing all three used to issue three identical reads.
type multiFileDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required"`
	A     string `json:"a"     db:"a"     mfx:"file"`
	B     string `json:"b"     db:"b"     mfx:"file"`
	C     string `json:"c"     db:"c"     mfx:"file"`
}

// countingAdapter delegates everything and tallies FindByID. Embedding the
// interface keeps it to the one method under test.
type countingAdapter struct {
	maniflex.DBAdapter
	findByID atomic.Int32
}

func (a *countingAdapter) FindByID(ctx context.Context, m *maniflex.ModelMeta, id string, q *maniflex.QueryParams) (any, error) {
	a.findByID.Add(1)
	return a.DBAdapter.FindByID(ctx, m, id, q)
}

func TestFileUpdate_ReadsThePreWriteRowOnce(t *testing.T) {
	t.Parallel()

	var counter *countingAdapter
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{multiFileDoc{}},
		FileStorage: store,
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			counter = &countingAdapter{DBAdapter: inner}
			return counter, nil
		},
	})

	// Create the row with all three files populated.
	create := srv.POSTMultipart("/multi_file_docs", map[string]string{"title": "doc"},
		map[string]testutil.FileUpload{
			"a": {Filename: "a1.txt", ContentType: "text/plain", Body: []byte("a1")},
			"b": {Filename: "b1.txt", ContentType: "text/plain", Body: []byte("b1")},
			"c": {Filename: "c1.txt", ContentType: "text/plain", Body: []byte("c1")},
		})
	create.AssertStatus(http.StatusCreated)
	id := create.ID()

	// Only the update matters — replacing all three auto_delete files, each of
	// which needs the key it replaces.
	counter.findByID.Store(0)
	upd := srv.PATCHMultipart("/multi_file_docs/"+id, nil,
		map[string]testutil.FileUpload{
			"a": {Filename: "a2.txt", ContentType: "text/plain", Body: []byte("a2")},
			"b": {Filename: "b2.txt", ContentType: "text/plain", Body: []byte("b2")},
			"c": {Filename: "c2.txt", ContentType: "text/plain", Body: []byte("c2")},
		})
	upd.AssertStatus(http.StatusOK)

	// The three fields share one read of the pre-write row. Anything that scales
	// with the field count means each field fetched the row for itself again.
	if got := counter.findByID.Load(); got > 1 {
		t.Errorf("the update issued %d FindByID reads of the same row; the three "+
			"auto_delete fields must share one pre-write read", got)
	}
}
