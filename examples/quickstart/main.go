// Command quickstart is the Quickstart from README.md, kept here as compiled
// code so that an API change cannot leave the first example a new user copies
// broken. It rotted once already: it carried the AutoMigrate field for a
// release after that field was replaced by DisableAutoMigrate (audit D2).
//
// doccheck syntax-checks Markdown fences but cannot type-check them, so a fence
// naming a field that no longer exists parses cleanly and ships. Only the
// compiler catches that, and it only sees real files.
//
// The region between the ANCHOR markers is compared byte-for-byte against the
// Quickstart fence in README.md by TestREADMEQuickstartMatchesCompiledExample.
// Edit the two together.
//
// Run with:
//
//	go run ./examples/quickstart
//
// ANCHOR: quickstart
package main

import (
	"log"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

type Post struct {
	maniflex.BaseModel
	Title  string `json:"title"  mfx:"required,filterable,sortable"`
	Body   string `json:"body"   mfx:"required"`
	Status string `json:"status" mfx:"required,filterable,enum:draft|published|archived"`
}

func main() {
	server := maniflex.New(maniflex.Config{
		Port:       8080,
		PathPrefix: "/api",
	})

	// Register models before opening the DB - the adapter needs the registry
	// to run migrations and resolve relations.
	server.MustRegister(Post{}, maniflex.ModelConfig{
		BaseModelTags: map[string]string{"created_at": "filterable,sortable"},
	})

	db, err := sqlite.Open("./blog.db", server.Registry())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	server.SetDB(db)

	log.Fatal(server.Start())
}

// ANCHOR_END: quickstart
