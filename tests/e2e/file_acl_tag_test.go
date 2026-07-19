package e2e

// 13.10 — an unrecognised mfx:"file_acl:" value must be refused, not quietly
// turned into private.
//
// The parser had cases for signed and public and a default that assigned
// private, so every typo landed on the safe-looking branch. Safe-looking is not
// the same as harmless: the author asked for signed or public URLs and got
// neither, so responses carry raw storage keys instead of URLs, and the reason
// appears nowhere. `upload:` in the same switch already reports its bad values.

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

func TestFileACLTag_UnknownValueIsRefused(t *testing.T) {
	t.Parallel()

	type aclTypo struct {
		maniflex.BaseModel
		Avatar string `json:"avatar" mfx:"file,file_acl:pubic"`
	}
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	err := srv.Register(aclTypo{})
	if err == nil {
		t.Fatal("expected an unrecognised file_acl value to be refused")
	}
	if !strings.Contains(err.Error(), "file_acl") {
		t.Errorf("the error should name the offending option, got: %v", err)
	}
}

// TestFileACLTag_EveryValidValueStillRegisters is the anti-over-reach pair, and
// it guards a specific trap: `private` used to be produced by the same default
// arm that swallowed typos, so it has no case of its own. A fix that only
// replaced the default would refuse the most ordinary spelling of all.
func TestFileACLTag_EveryValidValueStillRegisters(t *testing.T) {
	t.Parallel()

	for _, val := range []string{"private", "signed", "public"} {
		t.Run(val, func(t *testing.T) {
			t.Parallel()
			srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
			// Built per-case: the tag has to be a literal, so use a map of
			// pre-declared types keyed by value.
			if err := srv.Register(aclModelFor(val)); err != nil {
				t.Errorf("file_acl:%s must still register: %v", val, err)
			}
		})
	}
}

type aclPrivate struct {
	maniflex.BaseModel
	Avatar string `json:"avatar" mfx:"file,file_acl:private"`
}
type aclSigned struct {
	maniflex.BaseModel
	Avatar string `json:"avatar" mfx:"file,file_acl:signed"`
}
type aclPublic struct {
	maniflex.BaseModel
	Avatar string `json:"avatar" mfx:"file,file_acl:public"`
}

func aclModelFor(val string) any {
	switch val {
	case "private":
		return aclPrivate{}
	case "signed":
		return aclSigned{}
	}
	return aclPublic{}
}

// TestFileACLTag_AbsentTagIsStillPrivate: a field with no file_acl at all is
// unaffected — it never reaches the parse branch, and private remains the
// default for one that says nothing.
func TestFileACLTag_AbsentTagIsStillPrivate(t *testing.T) {
	t.Parallel()

	type aclNone struct {
		maniflex.BaseModel
		Avatar string `json:"avatar" mfx:"file"`
	}
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	if err := srv.Register(aclNone{}); err != nil {
		t.Fatalf("a file field with no file_acl must register: %v", err)
	}
	meta, ok := srv.Registry().Get("aclNone")
	if !ok {
		t.Fatal("aclNone not registered")
	}
	f := meta.FieldByJSONName("avatar")
	if f == nil {
		t.Fatal("avatar field not found")
	}
	if acl := f.Tags.FileACL; acl != "" && acl != maniflex.FileACLPrivate {
		t.Errorf("FileACL = %q, want private (or unset)", acl)
	}
}
