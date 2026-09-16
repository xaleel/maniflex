package maniflex

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

type Pipe6Memo struct {
	BaseModel `mfx:"versioned"`
	Title     string `json:"title"`
}

// Pipe6MemoHistory is an application's own model that happens to claim the name
// versioning needs for Pipe6Memo's history table.
type Pipe6MemoHistory struct {
	BaseModel
	Note string `json:"note"`
}

type Pipe6Plain struct {
	BaseModel `mfx:"versioned"`
	Title     string `json:"title"`
}

type Pipe6Other struct {
	BaseModel `mfx:"versioned"`
	Label     string `json:"label"`
}

func pipe6Server() *Server {
	return New(Config{
		PathPrefix:         "/api",
		DisableAutoMigrate: true,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// TestVersioning_HistoryNameTakenByAppModelIsRefused pins audit PIPE-6.
//
// The collision used to be swallowed as "already registered (e.g. called twice)
// — not fatal", which returned before registering any of the versioning
// middleware: the model recorded no history at all, and nothing said so. The
// "called twice" it excused is not a path that exists — registerVersioningFor
// has one call site, inside Register, which has already added the model itself
// by then.
func TestVersioning_HistoryNameTakenByAppModelIsRefused(t *testing.T) {
	srv := pipe6Server()
	if err := srv.Register(Pipe6MemoHistory{}); err != nil {
		t.Fatalf("register the application's own model: %v", err)
	}

	err := srv.Register(Pipe6Memo{})
	if err == nil {
		t.Fatal("Register accepted a versioned model whose history name is " +
			"already taken; versioning would be silently off for it")
	}
	// The message has to name both the model that cannot be versioned and the
	// name that is in the way — the operator's only move is to rename one.
	for _, want := range []string{"versioning setup for Pipe6Memo", "Pipe6MemoHistory", "rename"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// TestVersioning_AppModelTakingHistoryNameAfterwardsIsStillRefused is the
// must-still-work half: the opposite registration order has always been an
// error, and the fix is what makes the two orders agree.
func TestVersioning_AppModelTakingHistoryNameAfterwardsIsStillRefused(t *testing.T) {
	srv := pipe6Server()
	if err := srv.Register(Pipe6Memo{}); err != nil {
		t.Fatalf("register the versioned model: %v", err)
	}

	if err := srv.Register(Pipe6MemoHistory{}); err == nil {
		t.Fatal("Register accepted a model named after an existing history model")
	}
}

// TestVersioning_RegistersHistoryModelWhenNameIsFree is the must-still-work
// half: the ordinary case is untouched.
func TestVersioning_RegistersHistoryModelWhenNameIsFree(t *testing.T) {
	srv := pipe6Server()
	if err := srv.Register(Pipe6Plain{}, Pipe6Other{}); err != nil {
		t.Fatalf("register two versioned models: %v", err)
	}

	for _, name := range []string{"Pipe6PlainHistory", "Pipe6OtherHistory"} {
		hist, ok := srv.registry.Get(name)
		if !ok {
			t.Fatalf("history model %s was not registered", name)
		}
		if !hist.Config.Headless {
			t.Errorf("%s should be headless — it mounts no routes of its own", name)
		}
	}
}
