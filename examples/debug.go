//go:build ignore

// examples/debug.go demonstrates maniflex pipeline tracing — the structured,
// per-middleware DEBUG logging that lets you follow exactly what happens to a
// request as it moves through every step.
//
// # Running
//
//	go run examples/debug.go
//
// The server starts on :8081. Use the curl commands printed at startup to
// trigger different trace scenarios and observe the JSON log output.
//
// # What you'll see
//
// With Trace.Enabled and a DEBUG-level logger every request prints a log line
// for each middleware it passes through:
//
//	{"level":"DEBUG","msg":"middleware enter","step":"Auth","middleware":"BearerAuth","request_id":"..."}
//	{"level":"DEBUG","msg":"middleware exit", "step":"Auth","middleware":"BearerAuth","duration":"312µs","request_id":"..."}
//	{"level":"DEBUG","msg":"auth resolved","user_id":"usr_001","roles":["admin"],"request_id":"..."}
//	{"level":"DEBUG","msg":"query params parsed","page":1,"limit":20,"filters":0,"sorts":0,"request_id":"..."}
//	{"level":"DEBUG","msg":"parsed body fields","fields":["date_of_birth","name","status"],"request_id":"..."}
//	{"level":"DEBUG","msg":"transaction begin","request_id":"..."}
//	{"level":"DEBUG","msg":"DB step using active transaction","request_id":"..."}
//	{"level":"DEBUG","msg":"transaction commit","request_id":"..."}
//
// A request that fails auth shows the abort with call-site file:line:
//
//	{"level":"DEBUG","msg":"middleware exit","step":"Auth","middleware":"BearerAuth",
//	 "aborted_status":401,"aborted_code":"UNAUTHORIZED","abort_site":"debug.go:120","request_id":"..."}
//
// (build tag "ignore" keeps this file out of normal `go build ./...` runs)
package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// ── Models ────────────────────────────────────────────────────────────────────

// Person is a simple model to keep the example focused on tracing.
type Person struct {
	maniflex.BaseModel
	Name        string `json:"name"          mfx:"required,filterable,sortable"`
	DateOfBirth string `json:"date_of_birth" mfx:"required,filterable"`
	Status      string `json:"status"        mfx:"required,filterable,sortable,enum:active|discharged|pending"`
	Notes       string `json:"notes"         mfx:"sortable"`
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	// ── 1. Debug-capable logger ──────────────────────────────────────────────
	//
	// The default slog handler suppresses DEBUG records. Swap it for a JSON
	// handler with Level: slog.LevelDebug to see all pipeline trace output.
	// In production you would set Level: slog.LevelInfo (or higher) and all
	// trace output silently disappears with zero overhead.
	debugLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// ── 2. Server with full tracing ──────────────────────────────────────────
	//
	// Trace.Enabled activates Steps + Timings + Aborts.
	// Bodies is opt-in (may log sensitive fields) and set explicitly here so
	// we can see parsed body field names in the Deserialize step.
	server := maniflex.New(maniflex.Config{
		Port:   8081,
		Logger: debugLogger,
		Trace: maniflex.PipelineTrace{
			Enabled: true, // → Steps + Timings + Aborts
			Bodies:  true, // also log parsed body field names
		},
	})

	server.MustRegister(Person{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		log.Fatalf("sqlite: %v", err)
	}
	defer db.Close()
	server.SetDB(db)

	// ── 3. Named middleware registrations ────────────────────────────────────
	//
	// maniflex.WithName sets the name that appears as "middleware" in every trace
	// log record. Without it the field shows "[unnamed]", making it hard to
	// tell which middleware fired.

	// Auth: require a Bearer token for all writes.
	// Unauthenticated write requests will show the abort with call-site:
	//   "aborted_code":"UNAUTHORIZED","abort_site":"debug.go:120"
	server.Pipeline.Auth.Register(
		requireBearerToken,
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
		maniflex.WithName("BearerAuth"),
	)

	// Service: normalise the status field to lowercase before validation.
	server.Pipeline.Service.Register(
		normaliseStatus,
		maniflex.ForModel("Person"),
		maniflex.WithName("StatusNormaliser"),
	)

	// Service: wrap mutations in a transaction.
	// Trace will show: "transaction begin", "DB step using active transaction",
	// and "transaction commit" (or "transaction rollback" on error).
	server.Pipeline.Service.Register(
		maniflex.WithTransaction(nil),
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
		maniflex.WithName("WithTransaction"),
	)

	// Service: block Persons with status "pending" from being created on
	// weekends (contrived, but shows a named abort mid-pipeline).
	server.Pipeline.Service.Register(
		rejectPendingOnWeekends,
		maniflex.ForModel("Person"),
		maniflex.ForOperation(maniflex.OpCreate),
		maniflex.WithName("WeekendPendingGuard"),
	)

	// Response (After): stamp every response with a Trace-ID header so clients
	// can correlate their HTTP response to the DEBUG log stream.
	server.Pipeline.Response.Register(
		stampTraceID,
		maniflex.AtPosition(maniflex.After),
		maniflex.WithName("TraceIDHeader"),
	)

	printHelp()
	log.Fatal(server.Start())
}

// ── Middleware ────────────────────────────────────────────────────────────────

// requireBearerToken rejects write requests that have no Authorization header.
// When it fires on an unauthenticated request the trace will include:
//
//	"aborted_status":401, "aborted_code":"UNAUTHORIZED", "abort_site":"debug.go:NNN"
func requireBearerToken(ctx *maniflex.ServerContext, next func() error) error {
	header := ctx.Request.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
			"missing or invalid Authorization: Bearer <token> header")
		return nil
	}
	token := strings.TrimPrefix(header, "Bearer ")

	// Populate ctx.Auth — shows up in the "auth resolved" trace line:
	//   "user_id":"usr_001", "roles":["admin"]
	ctx.Auth = &maniflex.AuthInfo{
		UserID: "usr_001",
		Roles:  []string{"admin"},
		Claims: map[string]any{"token": token},
	}
	return next()
}

// normaliseStatus lowercases the status field so enum validation always passes
// regardless of how the client cased the value.
func normaliseStatus(ctx *maniflex.ServerContext, next func() error) error {
	if v, ok := ctx.Field("status"); ok {
		if s, ok := v.(string); ok {
			ctx.SetField("status", strings.ToLower(s))
		}
	}
	return next()
}

// rejectPendingOnWeekends is a contrived guard that shows a named abort
// mid-pipeline. The trace will show which named middleware triggered it and
// the exact source line of the ctx.Abort call.
func rejectPendingOnWeekends(ctx *maniflex.ServerContext, next func() error) error {
	// In this example we always allow — remove the early return to trigger
	// the abort and observe it in the trace output.
	return next()

	// ctx.Abort(http.StatusForbidden, "WEEKEND_RESTRICTION",
	//     "pending Persons cannot be admitted on weekends")
	// return nil
}

// stampTraceID copies the X-Request-Id into a friendlier X-Trace-Id header
// so clients can correlate their response to the DEBUG log lines.
func stampTraceID(ctx *maniflex.ServerContext, next func() error) error {
	if ctx.RequestID != "" {
		ctx.Writer.Header().Set("X-Trace-Id", ctx.RequestID)
	}
	return next()
}

// ── Help ──────────────────────────────────────────────────────────────────────

func printHelp() {
	log.Println("maniflex debug example — :8081")
	log.Println("All pipeline trace output is at DEBUG level (JSON to stdout).")
	log.Println()
	log.Println("── Scenario 1: anonymous GET — shows auth dump (anonymous) + query params ──")
	log.Println(`  curl -s http://localhost:8081/api/Persons | jq .`)
	log.Println()
	log.Println("── Scenario 2: authenticated POST — full trace including tx boundaries ─────")
	log.Println(`  curl -s -X POST http://localhost:8081/api/Persons \`)
	log.Println(`       -H "Authorization: Bearer dev-token" \`)
	log.Println(`       -H "Content-Type: application/json" \`)
	log.Println(`       -d '{"name":"Ada Lovelace","date_of_birth":"1815-12-10","status":"active"}' | jq .`)
	log.Println()
	log.Println("  Trace shows: auth resolved → parsed body fields → StatusNormaliser →")
	log.Println("               WithTransaction → transaction begin → DB step using active")
	log.Println("               transaction → transaction commit")
	log.Println("  (Note: exit records appear after the full chain unwinds — outer")
	log.Println("   middleware exit durations include all inner middleware time.)")
	log.Println()
	log.Println("── Scenario 3: missing token — shows abort with call-site ─────────────────")
	log.Println(`  curl -s -X POST http://localhost:8081/api/Persons \`)
	log.Println(`       -H "Content-Type: application/json" \`)
	log.Println(`       -d '{"name":"Bob"}' | jq .`)
	log.Println()
	log.Println(`  Trace shows: "aborted_status":401,"aborted_code":"UNAUTHORIZED",`)
	log.Println(`               "abort_site":"debug.go:NNN"`)
	log.Println()
	log.Println("── Scenario 4: filtered list — shows query params (filters + sorts) ────────")
	log.Println(`  curl -s "http://localhost:8081/api/Persons?filter=status:eq:active&sort=name:asc&limit=5" | jq .`)
	log.Println()
	log.Println(`  Trace shows: query params parsed  filters=1 sorts=1 page=1 limit=5`)
	log.Println()
	log.Println("── Tip: pipe through jq -R to separate JSON log lines from API responses ───")
	log.Println(`  go run examples/debug.go 2>/dev/null | jq -R 'try fromjson'`)
}
