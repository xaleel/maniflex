package e2e

// QF-2/QF-3 — a filter value is normalised against the column it targets,
// whether it arrived in a URL or was built in Go.

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// FilterTicket carries the two column kinds whose filter values need coercing:
// a bool (stored INTEGER on SQLite, BOOLEAN on Postgres) and a time.Time.
type FilterTicket struct {
	maniflex.BaseModel
	Title    string `json:"title"     db:"title"      mfx:"required,filterable"`
	Resolved bool   `json:"resolved"  db:"resolved"   mfx:"filterable,sortable"`
	OwnerID  string `json:"ownerId"   db:"owner_id"   mfx:"filterable"`
}

func filterTicketServer(t *testing.T) *testutil.Server {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{FilterTicket{}}})
	for _, row := range []map[string]any{
		{"title": "open-a", "resolved": false, "ownerId": "u1"},
		{"title": "open-b", "resolved": false, "ownerId": "u2"},
		{"title": "done-c", "resolved": true, "ownerId": "u1"},
	} {
		srv.POST("/filter_tickets", row).AssertStatus(http.StatusCreated)
	}
	return srv
}

// The bug as a client hits it: a JS caller interpolating a boolean into a
// template literal sends the word, and every layer accepts it — TypeScript, the
// generated client, and the API. The list just came back empty.
func TestFilterBoolWordMatchesRows(t *testing.T) {
	t.Parallel()
	srv := filterTicketServer(t)

	for _, spelling := range []string{"false", "0", "False"} {
		rows := srv.GET("/filter_tickets?filter=resolved:eq:" + spelling)
		rows.AssertStatus(http.StatusOK)
		if n := len(rows.DataList()); n != 2 {
			t.Fatalf("resolved:eq:%s returned %d rows, want 2", spelling, n)
		}
	}
	for _, spelling := range []string{"true", "1"} {
		rows := srv.GET("/filter_tickets?filter=resolved:eq:" + spelling)
		rows.AssertStatus(http.StatusOK)
		if n := len(rows.DataList()); n != 1 {
			t.Fatalf("resolved:eq:%s returned %d rows, want 1", spelling, n)
		}
	}
}

// neq must agree with eq rather than inverting a comparison that never matched.
func TestFilterBoolWordNeq(t *testing.T) {
	t.Parallel()
	srv := filterTicketServer(t)
	rows := srv.GET("/filter_tickets?filter=resolved:neq:false")
	rows.AssertStatus(http.StatusOK)
	if n := len(rows.DataList()); n != 1 {
		t.Fatalf("resolved:neq:false returned %d rows, want 1", n)
	}
}

// A set operator keeps its comma-separated shape, so each element has to be
// coerced on its own.
func TestFilterBoolWordIn(t *testing.T) {
	t.Parallel()
	srv := filterTicketServer(t)
	rows := srv.GET("/filter_tickets?filter=resolved:in:true,false")
	rows.AssertStatus(http.StatusOK)
	if n := len(rows.DataList()); n != 3 {
		t.Fatalf("resolved:in:true,false returned %d rows, want 3", n)
	}
}

// A filter built in Go — the shape a tenancy middleware or a hand-written
// action uses — may name its field either way, and may carry a real time.Time.
func TestFilterProgrammaticFieldAndValue(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{FilterTicket{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET",
				Path:   "/ticket-probe",
				Handler: func(ctx *maniflex.ServerContext) error {
					// "ownerId" is the json spelling; the column is owner_id.
					// A real time.Time bound against created_at must reach
					// SQLite in the canonical form the write path stored.
					rows, err := ctx.GetModel("FilterTicket").List(&maniflex.QueryParams{
						Page: 1, Limit: 50,
						Filters: []*maniflex.FilterExpr{
							{Field: "ownerId", Operator: maniflex.OpEq, Value: "u1", Group: -1},
							{Field: "created_at", Operator: maniflex.OpLte,
								Value: time.Now().Add(time.Hour), Group: -1},
						},
					})
					if err != nil {
						ctx.Abort(http.StatusInternalServerError, "PROBE", err.Error())
						return nil
					}
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"n": len(rows)},
					}
					return nil
				},
				AllowPublic: true,
			})
		},
	})
	for _, row := range []map[string]any{
		{"title": "a", "resolved": false, "ownerId": "u1"},
		{"title": "b", "resolved": false, "ownerId": "u2"},
		{"title": "c", "resolved": true, "ownerId": "u1"},
	} {
		srv.POST("/filter_tickets", row).AssertStatus(http.StatusCreated)
	}

	resp := srv.GET("/ticket-probe")
	resp.AssertStatus(http.StatusOK)
	if n, _ := resp.Data()["n"].(float64); int(n) != 2 {
		t.Fatalf("programmatic filter matched %v rows, want 2", resp.Data()["n"])
	}
}

// A Go-built filter naming a field the model does not have is a programming
// error. It used to reach the adapter verbatim and fail as an unknown-column
// SQL error; it must now say what is wrong.
func TestFilterProgrammaticUnknownFieldIsNamed(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{FilterTicket{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
					Field: "nope", Operator: maniflex.OpEq, Value: "x", Group: -1,
				})
				return next()
			})
		},
	})
	resp := srv.GET("/filter_tickets")
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("unknown filter field: status %d, want 500\n%s", resp.Status, resp.Body)
	}
	if code := resp.ErrorCode(); code != "INVALID_FILTER" {
		t.Fatalf("unknown filter field: error code %q, want INVALID_FILTER\n%s", code, resp.Body)
	}
}
