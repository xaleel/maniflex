//go:build ignore

// admin.go is a runnable example of the maniflex/admin panel — a server-rendered
// administration UI mounted alongside the generated REST API.
//
// Run:
//
//	go run examples/admin.go
//
// Then open:
//
//	http://localhost:8090/admin/      — the admin panel
//	http://localhost:8090/api/users   — the raw REST API it drives
//
// The panel is "just another API client": every page reads and writes data by
// issuing in-process HTTP calls against the server's own handler, so auth,
// validation, and the full pipeline apply unchanged.
package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/xaleel/maniflex/admin"
	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// Author writes articles. Exercises required / unique / enum tags.
type Author struct {
	maniflex.BaseModel
	Name string `json:"name"  mfx:"required,filterable,sortable"`
	Bio  string `json:"bio"`
	Tier string `json:"tier"  mfx:"filterable,enum:free|pro|staff,default:free"`
}

// Article belongs to an Author. Exercises FK relations, enum, soft-delete.
type Article struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title    string `json:"title"     mfx:"required,filterable,sortable"`
	Body     string `json:"body"      mfx:"required"`
	Status   string `json:"status"    mfx:"required,filterable,sortable,enum:draft|published"`
	AuthorID string `json:"author_id" mfx:"required,filterable"`
	Views    int    `json:"views"     mfx:"readonly,sortable"`
}

func main() {
	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(Author{}, Article{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	server.SetDB(db)
	if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
		log.Fatal(err)
	}
	seed(server)

	// Mount builds the panel handler. AllowUnauthenticated is set because this
	// is a local demo — in production supply Config.Auth with a real gate.
	panel := admin.Mount(server, admin.Config{
		Title:                "Publishing Admin",
		AllowUnauthenticated: true,
	})

	// One net/http mux serves the API and the panel side by side. No core
	// change and no chi needed: each handler owns its own path prefix.
	mux := http.NewServeMux()
	mux.Handle("/api/", server.Handler())
	mux.Handle("/admin/", panel)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	log.Println("admin panel:  http://localhost:8090/admin/")
	log.Println("rest api:     http://localhost:8090/api/authors")
	log.Fatal(http.ListenAndServe(":8090", mux))
}

// seed inserts a little demo data so the panel is not empty on first load.
func seed(server *maniflex.Server) {
	h := server.Handler()
	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/api/authors", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	post(`{"name":"Ada Lovelace","bio":"Computing pioneer","tier":"staff"}`)
	post(`{"name":"Grace Hopper","bio":"Compiler inventor","tier":"pro"}`)
}
