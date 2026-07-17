package maniflex

import (
	"strings"
	"testing"
)

func TestKeyScope_ApplyAndParseRoundTrip(t *testing.T) {
	scoped := applyKeyScope("tenant-42", "uploads/x/y.jpg")
	if !strings.HasPrefix(scoped, keyScopePrefix) {
		t.Fatalf("scoped key %q lacks the marker prefix", scoped)
	}
	if !strings.HasSuffix(scoped, "uploads/x/y.jpg") {
		t.Errorf("scoped key %q dropped the original key body", scoped)
	}
	seg, ok := keyScopeSegmentOf(scoped)
	if !ok {
		t.Fatalf("keyScopeSegmentOf(%q) reported no scope", scoped)
	}
	if seg != scopeSegment("tenant-42") {
		t.Errorf("embedded segment %q != hash of the scope %q", seg, scopeSegment("tenant-42"))
	}
}

func TestKeyScope_EmptyScopeLeavesKeyUntouched(t *testing.T) {
	if got := applyKeyScope("", "uploads/x"); got != "uploads/x" {
		t.Errorf("applyKeyScope(\"\", key) = %q, want the key unchanged", got)
	}
	if _, ok := keyScopeSegmentOf("uploads/x"); ok {
		t.Error("an unprefixed key should report no scope")
	}
	// A key that merely starts with something close to the marker is not scoped.
	if _, ok := keyScopeSegmentOf("mfxs1/"); ok {
		t.Error("a marker with no segment should report no scope")
	}
}

func TestKeyScope_DistinctScopesGetDistinctSegments(t *testing.T) {
	if scopeSegment("tenant-a") == scopeSegment("tenant-b") {
		t.Error("two distinct scopes collided to the same segment")
	}
	if scopeSegment("t") == "" {
		t.Error("scopeSegment produced an empty segment")
	}
}

func TestDefaultKeyScope_PrefersTenantThenUser(t *testing.T) {
	cases := []struct {
		name string
		ctx  *ServerContext
		want string
	}{
		{"tenant wins over user", &ServerContext{Auth: &AuthInfo{TenantID: "t", UserID: "u"}}, "t"},
		{"user when no tenant", &ServerContext{Auth: &AuthInfo{UserID: "u"}}, "u"},
		{"empty when anonymous", &ServerContext{}, ""},
		{"empty when ctx nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultKeyScope(tc.ctx); got != tc.want {
				t.Errorf("defaultKeyScope = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveKeyScope_UsesHookWhenSet(t *testing.T) {
	hook := func(*ServerContext) string { return "from-hook" }
	if got := resolveKeyScope(hook, nil); got != "from-hook" {
		t.Errorf("resolveKeyScope with hook = %q, want from-hook", got)
	}
	// nil hook falls back to the tenant/user default.
	ctx := &ServerContext{Auth: &AuthInfo{TenantID: "t"}}
	if got := resolveKeyScope(nil, ctx); got != "t" {
		t.Errorf("resolveKeyScope with nil hook = %q, want t (the default)", got)
	}
}
