package e2e

// Audit QRY-12 — the junction step of a many-to-many ?include= read the
// through-table raw: SELECT * FROM <junction> WHERE <local_fk> IN (...). Every
// other include level applies the related model's soft-delete condition and
// re-applies the request's forced scope; this one applied neither.
//
// So a link the application had soft-deleted — gone from the junction's own
// list — still materialised the related row under ?include=, carrying its
// deletion timestamp in _through. And a link row another tenant wrote between
// two of this tenant's records passed both endpoint checks, because both
// endpoints were this tenant's, and surfaced here with that tenant's payload.
// The entry called the scope half "mitigated because both endpoints are
// scoped"; that is exactly the condition a foreign link to your own records meets.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Soft delete ──────────────────────────────────────────────────────────────

type sdStudent struct {
	maniflex.BaseModel
	Name string `json:"name"`
}
type sdCourse struct {
	maniflex.BaseModel
	Title string `json:"title"`
}

// sdLink is the pure shape plus soft delete. deleted_at is in the link-table
// skip set, so this is still auto-detected as a junction.
type sdLink struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	SdStudentID string     `json:"sd_student_id" db:"sd_student_id" mfx:"relation"`
	SdStudent   *sdStudent `json:"sd_student,omitempty"`
	SdCourseID  string     `json:"sd_course_id"  db:"sd_course_id"  mfx:"relation"`
	SdCourse    *sdCourse  `json:"sd_course,omitempty"`
}

func includedIDs(t *testing.T, row map[string]any, key string) []string {
	t.Helper()
	list, ok := row[key].([]any)
	if !ok {
		t.Fatalf("%s is not a list: %#v", key, row[key])
	}
	ids := make([]string, 0, len(list))
	for _, v := range list {
		ids = append(ids, v.(map[string]any)["id"].(string))
	}
	return ids
}

func TestJunctionInclude_SoftDeletedLinkIsHidden(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{sdStudent{}, sdCourse{}, sdLink{}}})

	sid := srv.MustID(srv.POST("/sd_students", map[string]any{"name": "Ada"}))
	kept := srv.MustID(srv.POST("/sd_courses", map[string]any{"title": "Logic"}))
	gone := srv.MustID(srv.POST("/sd_courses", map[string]any{"title": "Rhetoric"}))
	srv.POST("/sd_links", map[string]any{"sd_student_id": sid, "sd_course_id": kept}).
		AssertStatus(http.StatusCreated)
	link := srv.MustID(srv.POST("/sd_links", map[string]any{"sd_student_id": sid, "sd_course_id": gone}))

	srv.DELETE("/sd_links/" + link).AssertStatus(http.StatusNoContent)

	// The junction's own list agreed the link was gone; the include did not.
	one := srv.GET("/sd_students/" + sid + "?include=sd_courses").AssertStatus(http.StatusOK).Data()
	if ids := includedIDs(t, one, "sd_courses"); len(ids) != 1 || ids[0] != kept {
		t.Errorf("include after soft-deleting a link = %v, want only %s — the deleted "+
			"link must not materialise its course", ids, kept)
	}

	// The reverse direction reads the same junction.
	rev := srv.GET("/sd_courses/" + gone + "?include=sd_students").AssertStatus(http.StatusOK).Data()
	if ids := includedIDs(t, rev, "sd_students"); len(ids) != 0 {
		t.Errorf("reverse include still reaches the student through a deleted link: %v", ids)
	}
}

// ── Scope ────────────────────────────────────────────────────────────────────

type scStudent struct {
	maniflex.BaseModel
	Name  string `json:"name"`
	OrgID string `json:"org_id" db:"org_id"`
}
type scCourse struct {
	maniflex.BaseModel
	Title string `json:"title"`
	OrgID string `json:"org_id" db:"org_id"`
}

// scEnrol is a tenant-partitioned junction with a payload of its own.
type scEnrol struct {
	maniflex.BaseModel
	maniflex.JunctionModel
	ScStudentID string     `json:"sc_student_id" db:"sc_student_id" mfx:"relation"`
	ScStudent   *scStudent `json:"sc_student,omitempty"`
	ScCourseID  string     `json:"sc_course_id"  db:"sc_course_id"  mfx:"relation"`
	ScCourse    *scCourse  `json:"sc_course,omitempty"`
	Grade       string     `json:"grade"`
	OrgID       string     `json:"org_id" db:"org_id"`
}

const foreignGrade = "tenant-b's note"

func scopedJunctionSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{scStudent{}, scCourse{}, scEnrol{}},
		Middleware: func(s *maniflex.Server) {
			for _, m := range []string{"scStudent", "scCourse", "scEnrol"} {
				s.Pipeline.DB.Register(orgScope(), maniflex.ForModel(m))
			}
		},
	})
}

func TestJunctionInclude_ForeignTenantLinkIsHidden(t *testing.T) {
	t.Parallel()
	srv := scopedJunctionSrv(t)

	sid := srv.MustID(srv.POST("/sc_students", map[string]any{"name": "Ada", "org_id": "tenant-a"}, asA))
	cid := srv.MustID(srv.POST("/sc_courses", map[string]any{"title": "Logic", "org_id": "tenant-a"}, asA))

	// Tenant B links two of A's records. The write is accepted — refusing it is
	// the write-side check PIPE-2 proposes for every BelongsTo a request names —
	// so the read side is what has to keep it out of A's responses.
	srv.POST("/sc_enrols", map[string]any{
		"sc_student_id": sid, "sc_course_id": cid, "grade": foreignGrade,
	}, asB).AssertStatus(http.StatusCreated)

	resp := srv.GET("/sc_students/"+sid+"?include=sc_courses", asA)
	resp.AssertStatus(http.StatusOK)
	if strings.Contains(string(resp.Body), foreignGrade) {
		t.Errorf("tenant B's link payload reached tenant A: %s", resp.Body)
	}
	if ids := includedIDs(t, resp.Data(), "sc_courses"); len(ids) != 0 {
		t.Errorf("A's student shows %v through a link A never made", ids)
	}
}

// The scope must not become a blanket wipe: A's own link, among B's, still
// shows with its payload — across a list include over two parents, so the
// scope's placeholder is bound after an IN list of more than one (SQLite binds
// positionally, and a misordered condition would put the tenant in an id slot).
func TestJunctionInclude_OwnLinksSurviveTheScope(t *testing.T) {
	t.Parallel()
	srv := scopedJunctionSrv(t)

	ada := srv.MustID(srv.POST("/sc_students", map[string]any{"name": "Ada", "org_id": "tenant-a"}, asA))
	bob := srv.MustID(srv.POST("/sc_students", map[string]any{"name": "Bob", "org_id": "tenant-a"}, asA))
	cid := srv.MustID(srv.POST("/sc_courses", map[string]any{"title": "Logic", "org_id": "tenant-a"}, asA))

	for _, sid := range []string{ada, bob} {
		srv.POST("/sc_enrols", map[string]any{
			"sc_student_id": sid, "sc_course_id": cid, "grade": "A", "org_id": "tenant-a",
		}, asA).AssertStatus(http.StatusCreated)
	}
	srv.POST("/sc_enrols", map[string]any{
		"sc_student_id": ada, "sc_course_id": cid, "grade": foreignGrade,
	}, asB).AssertStatus(http.StatusCreated)

	resp := srv.GET("/sc_students?include=sc_courses", asA)
	resp.AssertStatus(http.StatusOK)
	if strings.Contains(string(resp.Body), foreignGrade) {
		t.Errorf("tenant B's link payload reached tenant A: %s", resp.Body)
	}
	for _, row := range resp.DataList() {
		r := row.(map[string]any)
		courses, _ := r["sc_courses"].([]any)
		if len(courses) != 1 {
			t.Errorf("%s: %d courses, want exactly A's own link", r["name"], len(courses))
			continue
		}
		through, _ := courses[0].(map[string]any)["_through"].(map[string]any)
		if through["grade"] != "A" {
			t.Errorf("%s: _through = %#v, want A's own payload", r["name"], through)
		}
	}
}

// "When the junction carries the column": a link table with no tenant column is
// shared, not partitioned, and a scoped request still reads it — the same rule
// MS-9 applies to a related lookup table.
type scShared struct {
	maniflex.BaseModel
	ScStudentID string     `json:"sc_student_id" db:"sc_student_id" mfx:"relation"`
	ScStudent   *scStudent `json:"sc_student,omitempty"`
	ScCourseID  string     `json:"sc_course_id"  db:"sc_course_id"  mfx:"relation"`
	ScCourse    *scCourse  `json:"sc_course,omitempty"`
}

func TestJunctionInclude_UnpartitionedJunctionIsStillRead(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{scStudent{}, scCourse{}, scShared{}},
		Middleware: func(s *maniflex.Server) {
			for _, m := range []string{"scStudent", "scCourse"} {
				s.Pipeline.DB.Register(orgScope(), maniflex.ForModel(m))
			}
		},
	})

	sid := srv.MustID(srv.POST("/sc_students", map[string]any{"name": "Ada", "org_id": "tenant-a"}, asA))
	cid := srv.MustID(srv.POST("/sc_courses", map[string]any{"title": "Logic", "org_id": "tenant-a"}, asA))
	srv.POST("/sc_shareds", map[string]any{"sc_student_id": sid, "sc_course_id": cid}).
		AssertStatus(http.StatusCreated)

	data := srv.GET("/sc_students/"+sid+"?include=sc_courses", asA).AssertStatus(http.StatusOK).Data()
	if ids := includedIDs(t, data, "sc_courses"); len(ids) != 1 || ids[0] != cid {
		t.Errorf("include through an unpartitioned junction = %v, want [%s]", ids, cid)
	}
}
