// Package sql provides a SQL-backed implementation of middleware/auth.Revoker,
// so that a logout performed on one replica is seen by every other replica — and
// survives a restart, which an in-process blocklist does not.
//
// It is the option for a deployment that already has Postgres or SQLite and does
// not want a second piece of infrastructure for one small table. Where Redis is
// already in the stack, middleware/auth/redis does the same job with server-side
// key expiry and no sweeping; this package trades that for using the database the
// application already runs.
//
//	if err := authsql.Migrate(ctx, db, "postgres"); err != nil { ... }
//	rev := authsql.NewRevoker(db)
//	server.Pipeline.Auth.Register(auth.JWTAuth(secret, auth.JWTOptions{Revoker: rev}))
//	server.Action(auth.Logout(rev, ""))
//	server.Action(auth.LogoutAll(rev, "", 24*time.Hour))
//
// It lives in the core module because it needs nothing but database/sql — there
// is no driver dependency here, and none is imported.
package sql

import (
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ── Time representation ──────────────────────────────────────────────────────
//
// Every instant in these two tables is stored as Unix seconds in a BIGINT, not
// as a timestamp column. That is a deliberate departure from the rest of the
// framework, which writes fixed-width RFC3339 TEXT on SQLite and TIMESTAMPTZ on
// Postgres, and it is worth the inconsistency for two reasons.
//
// The first is that the domain is already second-resolution. Every value here is
// compared against a JWT registered claim — exp against the token's own expiry,
// cutoff against iat — and those claims are integer seconds by RFC 7519 §2. The
// framework itself reads them as `time.Unix(int64(f), 0)`. Storing more
// precision than the comparison can use would be decoration.
//
// The second is that an integer removes a whole class of divergence rather than
// managing it. A TEXT timestamp has to be fixed-width or SQLite's lexicographic
// TEXT comparison inverts order within a second (audit JB-7); a TIMESTAMPTZ comes
// back from lib/pq as a time.Time and from modernc as a string, so every read
// needs a two-branch scan. An integer compares numerically and scans as an int64
// on both drivers, with nothing to get wrong.
//
// Rounding is toward refusal in both directions: deadlines round up so an entry
// outlives the credential it blocks, and "now" rounds down so an entry is honoured
// through the whole second it expires in. A blocklist that is a second too
// generous refuses a token slightly too long; one that is a second too eager
// accepts a revoked token, which is the failure this type exists to prevent.

// unixCeil converts a deadline to Unix seconds, rounding up. A zero time yields
// no value at all — the column is NULL, meaning the entry never expires.
func unixCeil(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	secs := t.Unix()
	if t.Nanosecond() > 0 {
		secs++
	}
	return &secs
}

// unixFloor converts an instant to Unix seconds, rounding down. Used for "now"
// on the read path.
func unixFloor(t time.Time) int64 { return t.Unix() }

// ── Configuration ────────────────────────────────────────────────────────────

const (
	defaultTokenTable = "revoked_token"
	defaultUserTable  = "revoked_user"

	// defaultPruneEvery is how many writes pass between opportunistic sweeps,
	// matching auth.MemoryRevoker's interval. Reads filter on the deadline
	// regardless, so this only bounds the rows held by entries nobody asks about.
	defaultPruneEvery = 128
)

// tableIdentPattern restricts a table name to the conservative unquoted SQL
// identifier grammar shared by SQLite and Postgres. The name is interpolated
// straight into every statement and into the migration DDL, including into the
// derived index names as a bare substring — neither position accepts a bind
// parameter, so there is nothing to parameterise and rejecting outright beats
// escaping. Same reasoning as jobs/sql (audit JB-13).
const tableIdentPattern = `^[A-Za-z_][A-Za-z0-9_]*$`

var tableIdentRe = regexp.MustCompile(tableIdentPattern)

// Option configures a Revoker (and, via the same options, Migrate).
type Option func(*config)

type config struct {
	prefix     string
	driver     string // "" = auto-detect; "postgres" or "sqlite" to force
	pruneEvery int
}

func newConfig(opts []Option) (config, error) {
	c := config{pruneEvery: defaultPruneEvery}
	for _, o := range opts {
		o(&c)
	}
	if c.pruneEvery < 0 {
		c.pruneEvery = 0
	}
	for _, name := range []string{c.prefix + defaultTokenTable, c.prefix + defaultUserTable} {
		if !tableIdentRe.MatchString(name) {
			return c, fmt.Errorf(
				"middleware/auth/sql: invalid table name %q: must match %s",
				name, tableIdentPattern)
		}
	}
	return c, nil
}

func (c config) tokenTable() string { return c.prefix + defaultTokenTable }
func (c config) userTable() string  { return c.prefix + defaultUserTable }

// WithTablePrefix namespaces the two tables, so an application whose schema
// already owns those names — or which runs two independent blocklists — can move
// them. WithTablePrefix("auth_") gives "auth_revoked_token" and
// "auth_revoked_user". Index names are derived from the table names so they do
// not collide either.
//
// Pass the same option to both Migrate and NewRevoker. The resulting names must
// be plain SQL identifiers ([A-Za-z_][A-Za-z0-9_]*): they are interpolated
// directly into every statement, which cannot bind them as parameters. Migrate
// reports a bad one as an error and NewRevoker panics. Do not build this from
// user input.
func WithTablePrefix(prefix string) Option { return func(c *config) { c.prefix = prefix } }

// WithDriver forces the SQL dialect instead of detecting it from the *sql.DB's
// registered driver. Accepts "postgres"/"postgresql"/"pgx" and "sqlite"/"sqlite3";
// anything else falls through to detection rather than silently forcing one.
func WithDriver(name string) Option { return func(c *config) { c.driver = name } }

// WithPruneEvery sets how many writes pass between opportunistic sweeps of
// expired rows. Zero disables them, leaving Prune the only way rows are removed —
// choose that when write latency must be uniform, and schedule Prune yourself.
// Default: 128.
func WithPruneEvery(n int) Option { return func(c *config) { c.pruneEvery = n } }

// ── Revoker ──────────────────────────────────────────────────────────────────

// Revoker is a SQL-backed JWT blocklist for middleware/auth. It is safe for
// concurrent use.
//
// Two tables are used:
//
//	revoked_token(jti PRIMARY KEY, expires_at)     — one token, until its own exp
//	revoked_user (user_id PRIMARY KEY, cutoff, retain_until) — every token issued
//	                                                           before cutoff
//
// A NULL deadline in either table means "never drop", which is what a token
// carrying no exp claim produces. That is the safe direction: the alternative is
// dropping the entry while the token it blocks is still usable.
//
// Errors are returned rather than swallowed, which is what lets the middleware
// fail closed: during a database outage requests are refused with 503 instead of
// every revoked token quietly becoming valid again.
type Revoker struct {
	db   *stdsql.DB
	stmt statements

	pruneEvery int
	writes     atomic.Int64

	// nowFunc is swappable for tests.
	nowFunc func() time.Time
}

// NewRevoker returns a Revoker over db. Call Migrate once at startup first.
//
// It panics if WithTablePrefix produced a name that is not a plain SQL
// identifier: the name is interpolated into every statement this Revoker issues,
// so there is no safe way to continue — falling back to the default table would
// silently read and write the wrong blocklist. Migrate reports the same condition
// as an error, having a return value to report it with.
func NewRevoker(db *stdsql.DB, opts ...Option) *Revoker {
	c, err := newConfig(opts)
	if err != nil {
		panic(err)
	}
	return &Revoker{
		db:         db,
		stmt:       buildStatements(c, resolveIsPG(c.driver, db)),
		pruneEvery: c.pruneEvery,
		nowFunc:    time.Now,
	}
}

func (r *Revoker) now() time.Time {
	if r.nowFunc != nil {
		return r.nowFunc()
	}
	return time.Now()
}

// RevokeToken implements auth.Revoker.
//
// A repeat revocation of the same jti keeps the later deadline, and keeps NULL
// once either side is NULL, so re-revoking can only ever extend how long the
// entry is honoured.
func (r *Revoker) RevokeToken(ctx context.Context, jti string, expiresAt time.Time) error {
	if jti == "" {
		return errors.New("middleware/auth/sql: RevokeToken: empty jti")
	}
	if _, err := r.db.ExecContext(ctx, r.stmt.revokeToken, jti, unixCeil(expiresAt)); err != nil {
		return fmt.Errorf("middleware/auth/sql: revoke token: %w", err)
	}
	r.maybePrune(ctx)
	return nil
}

// IsTokenRevoked implements auth.Revoker.
//
// The deadline is applied in the WHERE clause rather than trusted to have been
// swept, so a row that Prune has not reached yet still stops being honoured at
// exactly the right moment. Correctness never depends on the sweep.
func (r *Revoker) IsTokenRevoked(ctx context.Context, jti string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, r.stmt.isTokenRevoked,
		jti, unixFloor(r.now())).Scan(&one)
	if errors.Is(err, stdsql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("middleware/auth/sql: is token revoked: %w", err)
	}
	return true, nil
}

// RevokeUser implements auth.Revoker.
//
// The cutoff is only ever moved forward, and unlike a read-then-write that rule
// is enforced by the statement itself: the upsert takes the greater of the stored
// and incoming values, so two concurrent revocations cannot resurrect tokens by
// landing out of order.
func (r *Revoker) RevokeUser(ctx context.Context, userID string, cutoff, retainUntil time.Time) error {
	if userID == "" {
		return errors.New("middleware/auth/sql: RevokeUser: empty userID")
	}
	if _, err := r.db.ExecContext(ctx, r.stmt.revokeUser,
		userID, *unixCeil(orNow(cutoff, r.now())), unixCeil(retainUntil)); err != nil {
		return fmt.Errorf("middleware/auth/sql: revoke user: %w", err)
	}
	r.maybePrune(ctx)
	return nil
}

// UserCutoff implements auth.Revoker.
func (r *Revoker) UserCutoff(ctx context.Context, userID string) (time.Time, error) {
	var secs int64
	err := r.db.QueryRowContext(ctx, r.stmt.userCutoff,
		userID, unixFloor(r.now())).Scan(&secs)
	if errors.Is(err, stdsql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("middleware/auth/sql: user cutoff: %w", err)
	}
	return time.Unix(secs, 0), nil
}

// orNow substitutes the current time for a zero cutoff. RevokeUser's cutoff is
// NOT NULL — "revoke nothing" is not a state this table can hold — so a caller
// passing the zero time means the same thing every caller means by it here:
// everything issued up to now.
func orNow(t, now time.Time) time.Time {
	if t.IsZero() {
		return now
	}
	return t
}

// Prune deletes every row past its deadline and reports how many went from each
// table. It is safe to call concurrently with normal traffic — a row still being
// honoured is never in range — and safe to call from several replicas at once.
//
// The Revoker also sweeps on its own every WithPruneEvery writes. Call this
// directly when you want the failure reported (the opportunistic sweep discards
// it, so housekeeping cannot fail a logout) or on a schedule of your own:
//
//	scheduled.Every(time.Hour, func(ctx context.Context) error {
//	    _, _, err := rev.Prune(ctx)
//	    return err
//	})
func (r *Revoker) Prune(ctx context.Context) (tokens, users int64, err error) {
	now := unixFloor(r.now())
	tokens, err = execCount(ctx, r.db, r.stmt.pruneTokens, now)
	if err != nil {
		return 0, 0, fmt.Errorf("middleware/auth/sql: prune tokens: %w", err)
	}
	users, err = execCount(ctx, r.db, r.stmt.pruneUsers, now)
	if err != nil {
		return tokens, 0, fmt.Errorf("middleware/auth/sql: prune users: %w", err)
	}
	return tokens, users, nil
}

// maybePrune runs the opportunistic sweep, discarding its error.
//
// The discard is the point rather than an oversight: this runs inside RevokeToken
// and RevokeUser, which are what /logout calls, and failing a logout because a
// housekeeping DELETE did not land would report the wrong thing to the user —
// their token *was* revoked. Nothing depends on the sweep for correctness, since
// every read filters on the deadline itself. Prune is exported for callers who
// want the error, and WithPruneEvery(0) turns this off entirely.
func (r *Revoker) maybePrune(ctx context.Context) {
	if r.pruneEvery <= 0 {
		return
	}
	if r.writes.Add(1)%int64(r.pruneEvery) != 0 {
		return
	}
	_, _, _ = r.Prune(ctx)
}

func execCount(ctx context.Context, db *stdsql.DB, query string, args ...any) (int64, error) {
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Not every driver reports it. The rows are gone either way.
		return 0, nil
	}
	return n, nil
}

// ── Statements ───────────────────────────────────────────────────────────────

type statements struct {
	revokeToken    string
	isTokenRevoked string
	revokeUser     string
	userCutoff     string
	pruneTokens    string
	pruneUsers     string
}

// buildStatements renders the six statements once, at construction.
//
// The dialects differ in exactly two places: the placeholder style, and the
// scalar maximum. SQLite spells it max(a,b) and Postgres GREATEST(a,b) — and they
// disagree on NULL, which matters here because NULL is a meaningful value in
// these columns rather than a missing one. SQLite's max returns NULL if any
// argument is NULL; Postgres's GREATEST ignores NULLs and returns the largest
// non-null. Since NULL means "never drop", SQLite's answer is the one we want,
// so both dialects wrap the call in a CASE that produces it explicitly. Relying
// on SQLite's behaviour and letting Postgres differ would silently give the
// Postgres deployment an entry that expires when it should have been permanent.
func buildStatements(c config, isPG bool) statements {
	tok, usr := c.tokenTable(), c.userTable()
	greatest := "max"
	if isPG {
		greatest = "GREATEST"
	}
	// nullableMax picks the later of two deadlines, preserving "never" (NULL).
	nullableMax := func(a, b string) string {
		return fmt.Sprintf(
			"CASE WHEN %[1]s IS NULL OR %[2]s IS NULL THEN NULL ELSE %[3]s(%[1]s, %[2]s) END",
			a, b, greatest)
	}

	s := statements{
		revokeToken: fmt.Sprintf(
			`INSERT INTO %[1]q ("jti", "expires_at") VALUES (?, ?)
			 ON CONFLICT ("jti") DO UPDATE SET "expires_at" = %[2]s`,
			tok, nullableMax(`"`+tok+`"."expires_at"`, `"excluded"."expires_at"`)),

		isTokenRevoked: fmt.Sprintf(
			`SELECT 1 FROM %[1]q WHERE "jti" = ? AND ("expires_at" IS NULL OR "expires_at" >= ?)`,
			tok),

		revokeUser: fmt.Sprintf(
			`INSERT INTO %[1]q ("user_id", "cutoff", "retain_until") VALUES (?, ?, ?)
			 ON CONFLICT ("user_id") DO UPDATE SET
			   "cutoff" = %[2]s(%[1]q."cutoff", "excluded"."cutoff"),
			   "retain_until" = %[3]s`,
			usr, greatest,
			nullableMax(`"`+usr+`"."retain_until"`, `"excluded"."retain_until"`)),

		userCutoff: fmt.Sprintf(
			`SELECT "cutoff" FROM %[1]q WHERE "user_id" = ? AND ("retain_until" IS NULL OR "retain_until" >= ?)`,
			usr),

		// Strictly less-than, so a row the read path would still honour is never
		// in range of the sweep.
		pruneTokens: fmt.Sprintf(
			`DELETE FROM %[1]q WHERE "expires_at" IS NOT NULL AND "expires_at" < ?`, tok),
		pruneUsers: fmt.Sprintf(
			`DELETE FROM %[1]q WHERE "retain_until" IS NOT NULL AND "retain_until" < ?`, usr),
	}

	if isPG {
		s.revokeToken = rebind(s.revokeToken)
		s.isTokenRevoked = rebind(s.isTokenRevoked)
		s.revokeUser = rebind(s.revokeUser)
		s.userCutoff = rebind(s.userCutoff)
		s.pruneTokens = rebind(s.pruneTokens)
		s.pruneUsers = rebind(s.pruneUsers)
	}
	return s
}

// rebind rewrites "?" placeholders to Postgres's $1..$n. The statements are
// fixed literals built above and contain no string constants, so a positional
// scan is exact.
func rebind(query string) string {
	var b strings.Builder
	n := 0
	for _, ch := range query {
		if ch == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// ── Migrate ──────────────────────────────────────────────────────────────────

// Migrate creates the two blocklist tables and their indexes if they do not
// exist. Call it once at startup, before NewRevoker. driver must be "postgres"
// or "sqlite". Pass the same options given to NewRevoker.
//
// It is safe to run on every boot and from several replicas at once: every
// statement is IF NOT EXISTS, and none of them rewrites an existing column.
func Migrate(ctx context.Context, db *stdsql.DB, driver string, opts ...Option) error {
	c, err := newConfig(opts)
	if err != nil {
		return err
	}
	tok, usr := c.tokenTable(), c.userTable()

	// BIGINT is the portable spelling: Postgres takes it as int8, and on SQLite
	// it lands in INTEGER affinity, so both compare and scan as an int64.
	ddl := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %[1]q (
  "jti"        TEXT   NOT NULL PRIMARY KEY,
  "expires_at" BIGINT NULL
)`, tok),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %[1]q (
  "user_id"      TEXT   NOT NULL PRIMARY KEY,
  "cutoff"       BIGINT NOT NULL,
  "retain_until" BIGINT NULL
)`, usr),
		// The sweep's access path. Without these, Prune degrades to a full scan of
		// a table whose whole purpose is to be read on every authenticated request.
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q ("expires_at")`,
			tok+"_expires_at", tok),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q ("retain_until")`,
			usr+"_retain_until", usr),
	}
	for _, stmt := range ddl {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("middleware/auth/sql: migrate: %w", err)
		}
	}
	return nil
}

// ── Driver detection ─────────────────────────────────────────────────────────

// resolveIsPG decides the dialect: an explicit WithDriver value wins, otherwise
// the driver is inspected. An unrecognised explicit value falls through to
// detection rather than silently forcing a dialect.
func resolveIsPG(explicit string, db *stdsql.DB) bool {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "postgres", "postgresql", "pgx":
		return true
	case "sqlite", "sqlite3":
		return false
	default:
		if db == nil {
			return false
		}
		return isPostgresDriver(driverIdent(reflect.TypeOf(db.Driver())))
	}
}

// driverIdent returns the import path and short type name of a driver type,
// unwrapping the pointer most drivers register. A pointer type has no PkgPath of
// its own, so the element must be taken first.
func driverIdent(t reflect.Type) (pkgPath, name string) {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil {
		return "", ""
	}
	return t.PkgPath(), t.String()
}

// isPostgresDriver classifies a driver as Postgres from its package path, which
// is stable across versions, with a short-name heuristic as a fallback. Matching
// on the name alone misreads jackc/pgx — whose driver is package "stdlib", type
// "stdlib.Driver" — as SQLite, and the adapter then speaks the wrong dialect
// throughout (audit JB-6, the same trap).
func isPostgresDriver(pkgPath, name string) bool {
	for _, m := range []string{"jackc/pgx", "lib/pq", "cockroachdb"} {
		if strings.Contains(pkgPath, m) {
			return true
		}
	}
	lower := strings.ToLower(name)
	return strings.Contains(lower, "pq") || strings.Contains(lower, "postgres")
}
