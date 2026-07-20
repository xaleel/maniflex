package e2e_test

// 11D.6 — when a broken many-to-many is discovered.
//
// The row asks for validation at Register rather than "on first request". Both
// halves need care:
//
//   - Register cannot do it. Resolution is a whole-registry operation: a
//     relation's target may be registered after the model pointing at it, so at
//     any single Register call the answer is legitimately "not yet". The
//     framework says so at strict.go's validateRegistry godoc.
//   - "On first request" is no longer where it happens. 10.1 moved
//     validateRegistry ahead of AutoMigrate in StartWithContext, so a broken
//     relation fails before the schema is touched, never mind before traffic.
//
// What is worth pinning is the property the row was really after: the failure is
// found at startup, and names the problem. These tests exist so it cannot drift
// back to being discovered lazily.

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

type m2mLeft struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

type m2mRight struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

// m2mDangling names a junction model that is never registered.
type m2mDangling struct {
	maniflex.BaseModel
	Name      string      `json:"name"`
	M2mRights []m2mRight  `json:"m2m_rights" mfx:"relation,through:NoSuchJunction"`
	Left      *m2mLeft    `json:"left,omitempty"`
}

// TestM2M_BrokenRelationFailsAtStartup: the misconfiguration must surface when
// the router is assembled, not when a request happens to touch the relation.
func TestM2M_BrokenRelationFailsAtStartup(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	// Register succeeds: at this point NoSuchJunction could still be registered
	// later, so refusing here would be wrong.
	if err := srv.Register(m2mLeft{}, m2mRight{}, m2mDangling{}); err != nil {
		t.Fatalf("Register should accept a forward reference: %v", err)
	}

	// Assembling the router is where the whole registry is known, and where it
	// must be rejected. Handler has no error return, so it panics.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a broken through: relation to fail when the router is assembled")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "NoSuchJunction") {
			t.Errorf("the error should name the missing junction, got: %v", r)
		}
	}()
	srv.Handler()
}

// m2mSource names its junction by a type registered after it. Package-level
// because the two reference each other, which a pair of function-local types
// cannot do.
type m2mSource struct {
	maniflex.BaseModel
	Name      string     `json:"name"`
	M2mRights []m2mRight `json:"m2m_rights" mfx:"relation,through:m2mJunction"`
}

type m2mJunction struct {
	maniflex.BaseModel
	M2mSourceID string     `json:"m2m_source_id" db:"m2m_source_id" mfx:"relation"`
	M2mSource   *m2mSource `json:"m2m_source,omitempty"`
	M2mRightID  string     `json:"m2m_right_id"  db:"m2m_right_id"  mfx:"relation"`
	M2mRight    *m2mRight  `json:"m2m_right,omitempty"`
}

// TestM2M_ForwardReferenceResolves is the anti-over-reach pair, and the reason
// Register cannot be the checkpoint: the junction is registered *after* the
// model that points at it, which must still work.
func TestM2M_ForwardReferenceResolves(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	// Deliberate order: the model naming the junction is registered first, so
	// at its own Register call the junction does not exist yet.
	if err := srv.Register(m2mSource{}, m2mRight{}, m2mJunction{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv.Handler() // must not panic
}
