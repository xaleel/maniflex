package e2e

// Phase 6 prerequisite: ?include= populates the typed companion/slice struct
// fields (Post.Comments []Comment), not just the extra carrier — so the typed
// read helpers expose related rows as concrete structs.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestTyped_IncludesPopulateStructFields(t *testing.T) {
	var (
		commentBodies []string
		gotComments   int
		hadTypedBody  bool
	)

	srv := testutil.NewServer(t, testutil.Options{
		Models: testutil.DefaultModels(),
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET", Path: "/typed-posts",
				Handler: func(ctx *maniflex.ServerContext) error {
					posts, err := maniflex.List[testutil.Post](ctx, &maniflex.QueryParams{
						Page: 1, Limit: 10, Includes: []string{"comments"},
					})
					if err != nil {
						return err
					}
					for _, p := range posts {
						if p.Title == "Hello" {
							gotComments = len(p.Comments)
							for _, c := range p.Comments {
								commentBodies = append(commentBodies, c.Body)
								if c.Approved || !c.Approved { // touch a typed bool field
									hadTypedBody = c.Body != ""
								}
							}
						}
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	uid := srv.MustID(srv.CreateUser("Ann", "ann@example.com", "editor"))
	pid := srv.MustID(srv.CreatePost("Hello", "published", uid))
	srv.CreateComment("first", pid, uid).AssertStatus(http.StatusCreated)
	srv.CreateComment("second", pid, uid).AssertStatus(http.StatusCreated)

	srv.GET("/typed-posts").AssertStatus(http.StatusOK)

	if gotComments != 2 {
		t.Fatalf("post.Comments len = %d, want 2 (typed include not populated)", gotComments)
	}
	if !hadTypedBody {
		t.Error("comment.Body not populated on the typed struct")
	}
	// Confirm the actual values came through typed.
	found := map[string]bool{}
	for _, b := range commentBodies {
		found[b] = true
	}
	if !found["first"] || !found["second"] {
		t.Errorf("typed comment bodies = %v, want first+second", commentBodies)
	}
}
