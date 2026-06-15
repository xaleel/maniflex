package e2e

// 4.2 — ?select= field projection. Only the requested columns are SELECTed
// from the DB; hidden/write-only fields are still stripped by the response
// step. Unknown field names return 400.

import (
	"net/http"
	"testing"

	"maniflex/tests/e2e/testutil"
)

func selectSrv(t *testing.T) (*testutil.Server, string, string) {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{})
	u1 := srv.MustID(srv.CreateUser("Alice", "alice@select.com", "admin"))
	u2 := srv.MustID(srv.CreateUser("Bob", "bob@select.com", "editor"))
	return srv, u1, u2
}

func TestSelect_ListProjectsRequestedFields(t *testing.T) {
	t.Parallel()
	srv, _, _ := selectSrv(t)

	items := srv.GET("/users?select=id,name").DataList()
	testutil.AssertLen(t, "users", items, 2)
	for _, raw := range items {
		m := raw.(map[string]any)
		if _, ok := m["id"]; !ok {
			t.Error("projected list: expected id field")
		}
		if _, ok := m["name"]; !ok {
			t.Error("projected list: expected name field")
		}
		// email was not selected — must be absent
		if _, ok := m["email"]; ok {
			t.Error("projected list: email must be absent when not selected")
		}
	}
}

func TestSelect_ReadProjectsRequestedFields(t *testing.T) {
	t.Parallel()
	srv, id, _ := selectSrv(t)

	m := srv.GET("/users/" + id + "?select=id,name,role").Data()
	if _, ok := m["id"]; !ok {
		t.Error("projected read: expected id field")
	}
	if _, ok := m["name"]; !ok {
		t.Error("projected read: expected name field")
	}
	if _, ok := m["role"]; !ok {
		t.Error("projected read: expected role field")
	}
	if _, ok := m["email"]; ok {
		t.Error("projected read: email must be absent when not selected")
	}
}

func TestSelect_UnknownFieldReturns400(t *testing.T) {
	t.Parallel()
	srv, _, _ := selectSrv(t)

	srv.GET("/users?select=nonexistent_field").AssertStatus(http.StatusBadRequest)
}

func TestSelect_EmptySelectReturnsAllFields(t *testing.T) {
	t.Parallel()
	srv, id, _ := selectSrv(t)

	// No ?select= — full row returned.
	m := srv.GET("/users/" + id).Data()
	for _, field := range []string{"id", "name", "email", "role"} {
		if _, ok := m[field]; !ok {
			t.Errorf("full read: expected field %q to be present", field)
		}
	}
}

func TestSelect_WriteonlyFieldStillStripped(t *testing.T) {
	t.Parallel()
	srv, id, _ := selectSrv(t)

	// password is mfx:"writeonly" — even if explicitly selected it must not
	// appear in the response (toJSONMap strips it).
	m := srv.GET("/users/" + id + "?select=id,name,password").Data()
	if _, ok := m["password"]; ok {
		t.Error("write-only field password must never appear in responses")
	}
}

func TestSelect_FilterAndSelectCombined(t *testing.T) {
	t.Parallel()
	srv, _, _ := selectSrv(t)

	// filter + select together — should still return only the projected fields.
	items := srv.GET("/users?filter=role:eq:admin&select=id,name").DataList()
	testutil.AssertLen(t, "admin users", items, 1)
	m := items[0].(map[string]any)
	if _, ok := m["name"]; !ok {
		t.Error("filtered+projected: expected name field")
	}
	if _, ok := m["email"]; ok {
		t.Error("filtered+projected: email must be absent")
	}
}

func TestSelect_SortAndSelectCombined(t *testing.T) {
	t.Parallel()
	srv, _, _ := selectSrv(t)

	// sort + select — sort works even on fields not in the projection.
	items := srv.GET("/users?sort=name:asc&select=id,name").DataList()
	testutil.AssertLen(t, "sorted users", items, 2)
	names := []string{
		testutil.Field(t, items[0].(map[string]any), "name"),
		testutil.Field(t, items[1].(map[string]any), "name"),
	}
	if names[0] > names[1] {
		t.Errorf("sort+select: want ascending order, got %v", names)
	}
}
