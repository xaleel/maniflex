//go:build ignore

// scheduled_example.go shows how to wire mfx:"scheduled" tags and a
// scheduled.Runner for time-driven state transitions.
//
// Run with:
//
//	go run ./examples/scheduled.go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/scheduled"
)

// Article soft-deletes when ExpiresAt passes, hard-deletes when PurgeAt
// passes, and flips to "published" when PublishAt passes (only if still
// "draft"). Three independent scheduled transitions on one model.
type Article struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt

	Title  string `json:"title"  mfx:"required"`
	Status string `json:"status" mfx:"enum:draft|published|archived,filterable"`

	PublishAt *time.Time `json:"publish_at" mfx:"scheduled;field=status;from=draft;to=published"`
	ExpiresAt *time.Time `json:"expires_at" mfx:"scheduled;soft-delete"`
	PurgeAt   *time.Time `json:"purge_at"   mfx:"scheduled;hard-delete"`
}

// Banner chains two scheduled color transitions: green → red → blue.
// Each step uses from= so the transitions are ordered and idempotent.
type Banner struct {
	maniflex.BaseModel

	Color        string     `json:"color"         mfx:"required,filterable"`
	HolidayStart *time.Time `json:"holiday_start" mfx:"scheduled;field=color;to=red"`
	HolidayEnd   *time.Time `json:"holiday_end"   mfx:"scheduled;field=color;from=red;to=blue"`
}

func main() {
	server := maniflex.New(maniflex.Config{
		PathPrefix:  "/api",
		AutoMigrate: true,
	})
	server.MustRegister(Article{}, Banner{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		log.Fatal(err)
	}
	server.SetDB(db)

	// Build the runner. New returns a no-op Runner when no model declares a
	// scheduled tag, so it is safe to wire unconditionally.
	runner, err := scheduled.New(server, scheduled.Config{
		Interval:  30 * time.Second,
		BatchSize: 500,
		OnDelete: func(model, id string) {
			fmt.Printf("[hook] deleted  model=%s id=%s\n", model, id)
		},
		OnSetField: func(model, id, field, to string) {
			fmt.Printf("[hook] set-field model=%s id=%s %s=%s\n", model, id, field, to)
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Start sweeps in the background. The first sweep fires immediately so a
	// just-booted replica drains any accumulated backlog.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.Start(ctx)

	fmt.Println("Listening on :8081  (POST /api/articles, /api/banners)")
	if err := http.ListenAndServe(":8081", server.Handler()); err != nil {
		log.Fatal(err)
	}
}
