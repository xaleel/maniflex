package e2e

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	_ "modernc.org/sqlite"
)

// ── Fixtures ─────────────────────────────────────────────────────────────────

type stOK struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

// stBadRelation carries mfx:"relation" on a field with no "ID" suffix, so the
// target model is inferred from the whole field name — always fatal.
type stBadRelation struct {
	maniflex.BaseModel
	Owner string `json:"owner" db:"owner" mfx:"relation"`
}

// stDanglingRelation names a target that is never registered — defensible (it
// may be a plain foreign id), so gated on Config.Strict.
type stDanglingRelation struct {
	maniflex.BaseModel
	GhostID string `json:"ghost_id" db:"ghost_id" mfx:"relation"`
}

// stBadScheduled declares mfx:"scheduled" with no action — a registration
// error, because a dropped scheduled field silently never sweeps.
type stBadScheduled struct {
	maniflex.BaseModel
	When *time.Time `json:"when" db:"when" mfx:"scheduled"`
}

// newStrictServer builds a server without booting it, so a startup failure can
// be observed without a listener.
func newStrictServer(t *testing.T, strict bool) *maniflex.Server {
	t.Helper()
	return maniflex.New(maniflex.Config{Strict: strict, Port: 0})
}

// recoverPanic runs fn and returns the panic message, or "" if it did not panic.
func recoverPanic(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			if s, ok := r.(string); ok {
				msg = s
			} else if e, ok := r.(error); ok {
				msg = e.Error()
			}
		}
	}()
	fn()
	return ""
}

// ── Registration-time failures (unconditional) ───────────────────────────────

// A ModelConfig with no model in front of it used to be dropped with a warning,
// so a config the author wrote silently did not apply.
func TestStrict_ModelConfigMisplacedIsRegistrationError(t *testing.T) {
	t.Parallel()

	t.Run("at position zero", func(t *testing.T) {
		srv := newStrictServer(t, false)
		err := srv.Register(maniflex.ModelConfig{TableName: "x"}, stOK{})
		if err == nil {
			t.Fatal("a ModelConfig at position 0 must be a registration error")
		}
		if !strings.Contains(err.Error(), "no model to attach to") {
			t.Errorf("error should explain the pairing rule: %v", err)
		}
	})

	t.Run("two in a row", func(t *testing.T) {
		srv := newStrictServer(t, false)
		err := srv.Register(stOK{},
			maniflex.ModelConfig{TableName: "a"},
			maniflex.ModelConfig{TableName: "b"})
		if err == nil {
			t.Fatal("two consecutive ModelConfigs must be a registration error")
		}
		if !strings.Contains(err.Error(), "would be dropped") {
			t.Errorf("error should say the second config would be lost: %v", err)
		}
	})

	t.Run("correct pairing still works", func(t *testing.T) {
		srv := newStrictServer(t, false)
		if err := srv.Register(stOK{}, maniflex.ModelConfig{TableName: "fine"}); err != nil {
			t.Fatalf("valid pairing must still register: %v", err)
		}
	})
}

func TestStrict_InvalidScheduledIsRegistrationError(t *testing.T) {
	t.Parallel()
	srv := newStrictServer(t, false)
	err := srv.Register(stBadScheduled{})
	if err == nil {
		t.Fatal("an invalid mfx:\"scheduled\" tag must be a registration error")
	}
	if !strings.Contains(err.Error(), "no action") {
		t.Errorf("error should name the specific problem: %v", err)
	}
}

// ── Handler-time failures ────────────────────────────────────────────────────

// The non-ID relation is fatal with strict off: the inferred target is a guess
// that is almost never a real model.
func TestStrict_BadRelationFailsWithoutStrict(t *testing.T) {
	t.Parallel()
	srv := newStrictServer(t, false)
	if err := srv.Register(stBadRelation{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg := recoverPanic(func() { srv.Handler() })
	if msg == "" {
		t.Fatal("Handler must fail for a relation on a field with no \"ID\" suffix")
	}
	if !strings.Contains(msg, "relation:Target") {
		t.Errorf("the failure should name the fix: %q", msg)
	}
}

// The dangling target is legal by default and fatal under strict.
func TestStrict_DanglingRelationIsGated(t *testing.T) {
	t.Parallel()

	t.Run("boots without strict", func(t *testing.T) {
		srv := newStrictServer(t, false)
		if err := srv.Register(stDanglingRelation{}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if msg := recoverPanic(func() { srv.Handler() }); msg != "" {
			t.Errorf("an unregistered relation target must not be fatal by default: %q", msg)
		}
	})

	t.Run("fails under strict", func(t *testing.T) {
		srv := newStrictServer(t, true)
		if err := srv.Register(stDanglingRelation{}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		msg := recoverPanic(func() { srv.Handler() })
		if msg == "" {
			t.Fatal("Config.Strict must reject a relation whose target is unregistered")
		}
		if !strings.Contains(msg, "Config.Strict") {
			t.Errorf("the failure should mark itself strict-only: %q", msg)
		}
	})
}

// Every problem in one report — the reason issues are collected rather than
// raised on sight.
func TestStrict_ReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	srv := maniflex.New(maniflex.Config{
		Strict:      true,
		Port:        0,
		StaticDir:   filepath.Join(t.TempDir(), "missing-assets"),
		FilesConfig: maniflex.FilesConfig{MountEndpoints: true},
	})
	if err := srv.Register(stBadRelation{}, stDanglingRelation{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg := recoverPanic(func() { srv.Handler() })
	if msg == "" {
		t.Fatal("expected a startup failure")
	}
	// Four independent problems: bad relation, dangling target, open /files,
	// missing static dir. Finding them one restart at a time is the thing this
	// replaces.
	for _, want := range []string{"startup problems", "relation:Target", "not registered",
		"BeforeMiddlewares", "missing-assets"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated failure missing %q:\n%s", want, msg)
		}
	}
}

// A correctly configured server is unaffected by any of this.
func TestStrict_ValidServerBootsUnderStrict(t *testing.T) {
	t.Parallel()
	srv := newStrictServer(t, true)
	if err := srv.Register(stOK{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if msg := recoverPanic(func() { srv.Handler() }); msg != "" {
		t.Errorf("a valid server must boot under Config.Strict: %q", msg)
	}
}

// ── Boot ordering ────────────────────────────────────────────────────────────

// Validation is hoisted ahead of migrate (10.1). Previously the checks lived in
// the Handler call that StartWithContext makes *after* migrating, so a
// configuration error was discovered only once the schema had been altered.
func TestStrict_FailsBeforeMigrating(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "strict.db")
	srv := maniflex.New(maniflex.Config{Port: 0})
	// A model that is fine to register but fails the whole-registry check.
	if err := srv.Register(stBadRelation{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	adapter, err := sqlite.Open(dbPath, srv.Registry())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	// Registered after t.TempDir so it closes first — Windows will not remove a
	// file the adapter still holds open.
	t.Cleanup(func() {
		if c, ok := adapter.(interface{ Close() error }); ok {
			c.Close()
		}
	})
	srv.SetDB(adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	startErr := srv.StartWithContext(ctx)
	if startErr == nil {
		t.Fatal("StartWithContext must return the configuration error")
	}
	if !strings.Contains(startErr.Error(), "relation:Target") {
		t.Errorf("Start should return the startup problem, got: %v", startErr)
	}

	// The teeth: nothing was migrated. If validation still ran after migrate,
	// the table would exist by now.
	if _, err := os.Stat(dbPath); err == nil {
		raw, openErr := sql.Open("sqlite", dbPath)
		if openErr != nil {
			t.Fatalf("open db: %v", openErr)
		}
		defer raw.Close()
		var n int
		queryErr := raw.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='st_bad_relations'`,
		).Scan(&n)
		if queryErr != nil {
			t.Fatalf("query sqlite_master: %v", queryErr)
		}
		if n != 0 {
			t.Error("the schema was migrated before configuration was validated — " +
				"a config error must cost nothing to find")
		}
	}
}

// StartWithContext returns the error rather than panicking; Handler, which has
// no error channel, panics. Both must report the same problem.
func TestStrict_StartReturnsErrorHandlerPanics(t *testing.T) {
	t.Parallel()

	build := func() *maniflex.Server {
		s := newStrictServer(t, false)
		if err := s.Register(stBadRelation{}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		return s
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	startErr := build().StartWithContext(ctx)
	if startErr == nil {
		t.Fatal("StartWithContext should return the error, not panic")
	}

	panicMsg := recoverPanic(func() { build().Handler() })
	if panicMsg == "" {
		t.Fatal("Handler should panic — it has no error return")
	}
	if !strings.Contains(startErr.Error(), "relation:Target") ||
		!strings.Contains(panicMsg, "relation:Target") {
		t.Errorf("both paths must report the same problem:\n start: %v\n handler: %s",
			startErr, panicMsg)
	}
}
