//go:build !linux && !darwin

package githubactions

// osPathLocked always reports false here: on Windows an ACL, not a mode and owner, says who may
// write, and this check does not read ACLs, so no cache entry is trusted without the admin's
// --trust-action-cache.
func osPathLocked(string) bool {
	return false
}
