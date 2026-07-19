package e2e

// MS-L9 / MS-L10: which models are join tables, and what that implies.
//
// Auto-detection used to accept any model with two BelongsTo to distinct models,
// so an entity with two foreign keys silently gained a many-to-many between its
// endpoints. It is now narrowed to the one unambiguous shape — nothing but the
// two keys — and anything else declares itself with maniflex.JunctionModel.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Models ───────────────────────────────────────────────────────────────────

type jnStudent struct {
	maniflex.BaseModel
	Name string `json:"name"`
}
type jnCourse struct {
	maniflex.BaseModel
	Title string `json:"title"`
}

// jnLink is the pure shape: two keys and nothing else. Auto-detected.
type jnLink struct {
	maniflex.BaseModel
	JnStudentID string     `json:"jn_student_id" db:"jn_student_id" mfx:"relation"`
	JnStudent   *jnStudent `json:"jn_student,omitempty"`
	JnCourseID  string     `json:"jn_course_id"  db:"jn_course_id"  mfx:"relation"`
	JnCourse    *jnCourse  `json:"jn_course,omitempty"`
}

// jnEnrollment carries a column of its own, so it is NOT auto-detected — this is
// the entity case the old rule got wrong. It declares itself, and does not
// declare uniqueness: the same student may enrol on the same course in two terms.
type jnEnrollment struct {
	maniflex.BaseModel
	maniflex.JunctionModel
	JnStudentID string     `json:"jn_student_id" db:"jn_student_id" mfx:"relation"`
	JnStudent   *jnStudent `json:"jn_student,omitempty"`
	JnCourseID  string     `json:"jn_course_id"  db:"jn_course_id"  mfx:"relation"`
	JnCourse    *jnCourse  `json:"jn_course,omitempty"`
	Term        string     `json:"term"`
}

// jnMembership declares the pair unique — a pure link table with payload.
type jnMembership struct {
	maniflex.BaseModel
	maniflex.JunctionModel `mfx:"unique"`
	JnStudentID            string     `json:"jn_student_id" db:"jn_student_id" mfx:"relation"`
	JnStudent              *jnStudent `json:"jn_student,omitempty"`
	JnCourseID             string     `json:"jn_course_id"  db:"jn_course_id"  mfx:"relation"`
	JnCourse               *jnCourse  `json:"jn_course,omitempty"`
	Role                   string     `json:"role"`
}

// jnOrder is the audit's example: an entity with two foreign keys and a payload
// column. It must not become a junction, and needs no opt-out to avoid it.
type jnOrder struct {
	maniflex.BaseModel
	JnStudentID string     `json:"jn_student_id" db:"jn_student_id" mfx:"relation"`
	JnStudent   *jnStudent `json:"jn_student,omitempty"`
	JnCourseID  string     `json:"jn_course_id"  db:"jn_course_id"  mfx:"relation"`
	JnCourse    *jnCourse  `json:"jn_course,omitempty"`
	Total       int        `json:"total"`
}

// m2mCount reports how many many-to-many relations were registered on jnStudent.
func m2mCount(t *testing.T, models ...any) int {
	t.Helper()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(models...)
	_ = srv.Handler() // resolves many-to-many
	meta, ok := srv.Registry().Get("jnStudent")
	if !ok {
		t.Fatalf("jnStudent not registered")
	}
	n := 0
	for _, r := range meta.Relations {
		if r.Kind == maniflex.ManyToMany {
			n++
		}
	}
	return n
}

// TestJunction_WhichModelsAreJoinTables is the whole decision table in one place.
func TestJunction_WhichModelsAreJoinTables(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		extra []any
		want  int
	}{
		// The unambiguous shape still needs no declaration.
		{"pure_link_auto_detected", []any{jnLink{}}, 1},
		// The whole point of MS-L9: payload means it is not obviously a join
		// table, so no relation is invented.
		{"entity_with_two_fks_is_not_a_junction", []any{jnOrder{}}, 0},
		// ...and the same shape becomes one by saying so.
		{"payload_junction_declares_itself", []any{jnEnrollment{}}, 1},
		// The opt-out still works on the shape that would be auto-detected.
		{"pure_link_opted_out", []any{jnLink{}, maniflex.ModelConfig{DisableAutoJunction: true}}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			models := append([]any{jnStudent{}, jnCourse{}}, tc.extra...)
			if got := m2mCount(t, models...); got != tc.want {
				t.Errorf("m2m relations on jnStudent: got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestJunction_MarkerRequiresAPair refuses the marker where there is no pair to
// join, rather than silently declaring nothing.
func TestJunction_MarkerRequiresAPair(t *testing.T) {
	t.Parallel()

	type jnBadMarker struct {
		maniflex.BaseModel
		maniflex.JunctionModel
		Name string `json:"name"`
	}
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	err := srv.Register(jnBadMarker{})
	if err == nil {
		t.Fatalf("expected JunctionModel without two BelongsTo to be refused")
	}
	if !strings.Contains(err.Error(), "exactly two BelongsTo") {
		t.Errorf("error should explain the requirement, got: %v", err)
	}
}

// TestJunction_UnknownEmbedOption catches a typo in the embed tag instead of
// silently ignoring it — the failure mode MS-L11 was about.
func TestJunction_UnknownEmbedOption(t *testing.T) {
	t.Parallel()

	type jnTypo struct {
		maniflex.BaseModel
		maniflex.JunctionModel `mfx:"uniqe"`
		JnStudentID            string     `json:"jn_student_id" db:"jn_student_id" mfx:"relation"`
		JnStudent              *jnStudent `json:"jn_student,omitempty"`
		JnCourseID             string     `json:"jn_course_id"  db:"jn_course_id"  mfx:"relation"`
		JnCourse               *jnCourse  `json:"jn_course,omitempty"`
	}
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	if err := srv.Register(jnTypo{}); err == nil {
		t.Fatal("expected an unknown JunctionModel option to be refused")
	}
}

// ── mfx:"unique" ─────────────────────────────────────────────────────────────

// TestJunction_UniqueEnforcedAtTheDatabase: declaring uniqueness must actually
// create the index, not merely record an intention.
func TestJunction_UniqueEnforcedAtTheDatabase(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{jnStudent{}, jnCourse{}, jnMembership{}},
	})
	sid := srv.MustID(srv.POST("/jn_students", map[string]any{"name": "Ada"}))
	cid := srv.MustID(srv.POST("/jn_courses", map[string]any{"title": "Logic"}))

	link := map[string]any{"jn_student_id": sid, "jn_course_id": cid, "role": "student"}
	srv.POST("/jn_memberships", link).AssertStatus(http.StatusCreated)
	// Same pair again — the UNIQUE index must refuse it.
	if got := srv.POST("/jn_memberships", link).Status; got != http.StatusConflict {
		t.Errorf("duplicate link: got %d, want 409 from the unique index", got)
	}
}

// TestJunction_NoUniqueByDefault is the anti-over-reach pair. jnEnrollment is a
// junction and does NOT declare uniqueness, so the same pair must be storable —
// one enrollment per term. A fix that made uniqueness implicit fails here.
func TestJunction_NoUniqueByDefault(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{jnStudent{}, jnCourse{}, jnEnrollment{}},
	})
	sid := srv.MustID(srv.POST("/jn_students", map[string]any{"name": "Ada"}))
	cid := srv.MustID(srv.POST("/jn_courses", map[string]any{"title": "Logic"}))

	srv.POST("/jn_enrollments", map[string]any{
		"jn_student_id": sid, "jn_course_id": cid, "term": "2026-spring",
	}).AssertStatus(http.StatusCreated)
	srv.POST("/jn_enrollments", map[string]any{
		"jn_student_id": sid, "jn_course_id": cid, "term": "2026-autumn",
	}).AssertStatus(http.StatusCreated)

	// Both survive the include: the pair repeats legitimately, so collapsing
	// them would drop a term — and each carries its own _through payload.
	data := srv.GET("/jn_students/" + sid + "?include=jn_courses").
		AssertStatus(http.StatusOK).Data()
	courses, _ := data["jn_courses"].([]any)
	if len(courses) != 2 {
		t.Fatalf("include: got %d courses, want 2 (one per enrollment)", len(courses))
	}
	terms := map[string]bool{}
	for _, c := range courses {
		if th, ok := c.(map[string]any)["_through"].(map[string]any); ok {
			terms[th["term"].(string)] = true
		}
	}
	if !terms["2026-spring"] || !terms["2026-autumn"] {
		t.Errorf("each link must keep its own _through payload, got %v", terms)
	}
}

// ── Cascade ──────────────────────────────────────────────────────────────────

// TestJunction_DeletingAnEndpointCollectsLinks: a link to a row that no longer
// exists says nothing, and leaving it behind is how junction tables accumulated
// orphans (audit MS-L10).
func TestJunction_DeletingAnEndpointCollectsLinks(t *testing.T) {
	t.Parallel()

	var remaining int
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{jnStudent{}, jnCourse{}, jnLink{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET", Path: "/jn_links/count",
				Handler: func(ctx *maniflex.ServerContext) error {
					rows, err := ctx.RawQuery("SELECT COUNT(*) AS n FROM jn_links")
					if err != nil {
						return err
					}
					remaining = int(jnCount(rows[0]["n"]))
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
					return nil
				},
			})
		},
	})

	sid := srv.MustID(srv.POST("/jn_students", map[string]any{"name": "Ada"}))
	cid := srv.MustID(srv.POST("/jn_courses", map[string]any{"title": "Logic"}))
	srv.POST("/jn_links", map[string]any{"jn_student_id": sid, "jn_course_id": cid}).
		AssertStatus(http.StatusCreated)

	srv.DELETE("/jn_students/" + sid).AssertStatus(http.StatusNoContent)

	srv.GET("/jn_links/count").AssertStatus(http.StatusOK)
	if remaining != 0 {
		t.Errorf("deleting an endpoint left %d orphaned link row(s)", remaining)
	}
}

// jnCount coerces a COUNT(*) result, which arrives typed by driver.
func jnCount(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return -1
}
