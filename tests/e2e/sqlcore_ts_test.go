package e2e_test

// NEW-1 (db/sqlcore sibling of jobs/sql audit JB-7): SQLite compares timestamp
// TEXT columns byte-by-byte, so a range filter over a time column only orders rows
// correctly if the stored form and the filter bound are both fixed-width. The write
// path now stores time.Time in that form, and ParseFilterParam canonicalises a
// well-formed timestamp bound into it. These tests drive the whole pipeline over
// HTTP against a real SQLite database, at the whole-second/fractional boundary the
// old variable-width RFC3339Nano form got wrong.
//
//	go test ./e2e/ -run TestSQLCoreTimestamp

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/db/sqlite"
)

// TimeEvent carries a client-settable, filterable time column — the shape a
// ?filter=event_at:lte:<ts> range query runs against.
type TimeEvent struct {
	maniflex.BaseModel
	EventAt time.Time `json:"event_at" db:"event_at" mfx:"filterable"`
	Label   string    `json:"label"    db:"label"`
}

func tsSrv(t *testing.T) string {
	t.Helper()
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })

	server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	server.MustRegister(TimeEvent{})
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	server.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func tsReq(t *testing.T, base, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, _ := http.NewRequest(method, base+"/api/time_events"+path, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A row whose event_at is on a whole second must be selected by a `lte` bound a
// fraction later in the same second — the case the old variable-width form got
// wrong (the stored whole-second string sorted after the fractional bound).
func TestSQLCoreTimestamp_WholeSecondRowMatchesFractionalUpperBound(t *testing.T) {
	base := tsSrv(t)

	if code, body := tsReq(t, base, "POST", "",
		`{"event_at":"2026-07-21T12:34:56Z","label":"whole"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}

	code, body := tsReq(t, base, "GET", "?filter=event_at:lte:2026-07-21T12:34:56.5Z", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	if !strings.Contains(body, `"whole"`) {
		t.Errorf("whole-second row was not returned by an lte bound half a second later "+
			"(due row looks not-due by up to a second): %s", body)
	}
}

// The inverse: a row a fraction into a second must NOT be selected by an upper
// bound on the whole second earlier in that same second — no matching before time.
func TestSQLCoreTimestamp_FractionalRowExcludedByWholeSecondUpperBound(t *testing.T) {
	base := tsSrv(t)

	if code, body := tsReq(t, base, "POST", "",
		`{"event_at":"2026-07-21T12:34:56.5Z","label":"frac"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}

	code, body := tsReq(t, base, "GET", "?filter=event_at:lte:2026-07-21T12:34:56Z", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	if strings.Contains(body, `"frac"`) {
		t.Errorf("a row at .5s was matched by an lte bound at .0s of the same second "+
			"(future row looks due by up to a second): %s", body)
	}
}
