package maniflex

// file_scope.go — principal-scoped storage keys (P1-20).
//
// A record may reference a file by naming an already-stored storage key. That
// check proved only that the object existed, never that the caller was allowed
// to use it — so any leaked key (they travel in signed URLs and file_acl:private
// responses) could be pinned onto another caller's record, and an auto_delete
// field could then delete a blob its record never owned.
//
// The fix binds a key to the principal that mints it. Every minting path prefixes
// the key with a hash of the caller's scope token; the reference paths recompute
// the token and refuse a scoped key whose hash is not the caller's. A key with no
// scope marker — minted before this release, or for an anonymous caller — is left
// to the existence check, so the change closes the hole for keys minted under a
// principal without breaking references to keys that predate it.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// keyScopePrefix marks a storage key the framework has bound to a principal. It
// is version-tagged so the scheme can change without misreading an old key, and
// distinctive enough that a real KeyGen layout ("uploads/…", "<table>/…") will
// not be mistaken for a scoped one.
const keyScopePrefix = "mfxs1/"

// defaultKeyScope derives the owner of a file key from the request's principal:
// the tenant when present (so a tenant's members share one scope), else the user.
// An anonymous request yields "" — nothing to bind to, so the key is left
// unscoped.
func defaultKeyScope(ctx *ServerContext) string {
	if ctx == nil || ctx.Auth == nil {
		return ""
	}
	if ctx.Auth.TenantID != "" {
		return ctx.Auth.TenantID
	}
	return ctx.Auth.UserID
}

// resolveKeyScope returns the caller's scope token, using the configured hook
// when set and the tenant/user default otherwise.
func resolveKeyScope(fn func(*ServerContext) string, ctx *ServerContext) string {
	if fn != nil {
		return fn(ctx)
	}
	return defaultKeyScope(ctx)
}

// scopeSegment hashes a raw scope token into the fixed-width, URL-safe segment
// embedded in a key. Hashing keeps the raw tenant/user id out of the key (and so
// out of signed URLs and error messages) and bounds the segment length; 64 bits
// is ample against accidental collision between two distinct tenants.
func scopeSegment(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:8])
}

// applyKeyScope prefixes key with scope's marker so a later reference can be
// checked against the referencing principal. An empty scope leaves the key
// untouched — there is nothing to bind it to.
func applyKeyScope(scope, key string) string {
	if scope == "" {
		return key
	}
	return keyScopePrefix + scopeSegment(scope) + "/" + key
}

// keyScopeSegmentOf returns the embedded scope segment of a scoped key, or
// ("", false) when the key carries no scope marker (legacy or anonymous).
func keyScopeSegmentOf(key string) (string, bool) {
	if !strings.HasPrefix(key, keyScopePrefix) {
		return "", false
	}
	rest := key[len(keyScopePrefix):]
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}
