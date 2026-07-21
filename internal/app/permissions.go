package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/looprig/tools/permission"
)

// permissions.go owns CodeRig's product permission policy: the automatic Bash
// family eligibility catalog and the permission-file location rules. The catalog
// is product policy (a catalog change updates the durable configuration
// fingerprint through the profile/loop revisions). The interactive default file
// path is the ONLY place CodeRig touches HOME; headless runs accept an explicit
// read-only path and never search HOME.

// permissionFileName is the fixed workspace permission file name.
const permissionFileName = "permissions.json"

// productFamilyEligibility returns CodeRig's v1 automatic Bash-family
// eligibility catalog predicate. Its positive set is EXACTLY the five safe,
// non-exec-capable git subcommands: `git log`, `git status`, `git diff`,
// `git show`, and `git push`. Every other prefix (including `git`, `git commit`,
// and `git logs`) is out of catalog, so an unknown execution wrapper can never
// become family-approved automatically. The predicate is consulted with a
// specific candidate token prefix and returns true only for an exact catalog
// member.
func productFamilyEligibility() permission.FamilyEligibility {
	catalog := map[string]struct{}{
		"git log":    {},
		"git status": {},
		"git diff":   {},
		"git show":   {},
		"git push":   {},
	}
	return func(tokens []string) bool {
		if len(tokens) != 2 {
			return false
		}
		_, ok := catalog[tokens[0]+" "+tokens[1]]
		return ok
	}
}

// defaultPermissionsPath computes CodeRig's interactive workspace permission
// file:
//
//	~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json
//
// The file lives outside the repository and is the sole destination for
// "Approve always for this workspace". This function is used ONLY in interactive
// CodeRig assembly. canonicalWorkspace must be the absolute, canonical workspace
// root (the same value the profile is built on); a relative or empty value fails
// closed. A home directory that cannot be resolved fails loud rather than
// falling back to a surprising path.
func defaultPermissionsPath(canonicalWorkspace string) (string, error) {
	if canonicalWorkspace == "" || !filepath.IsAbs(canonicalWorkspace) {
		return "", fmt.Errorf("coderig: permission-file path requires a canonical workspace root, got %q", canonicalWorkspace)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &StoreInitError{Stage: "permission-home", Cause: err}
	}
	digest := sha256.Sum256([]byte(canonicalWorkspace))
	return filepath.Join(home, ".looprig", "workspaces", hex.EncodeToString(digest[:]), permissionFileName), nil
}

// interactivePermissionConfig builds the interactive workspace permission-store
// Config: the HOME-derived per-workspace file plus CodeRig's family catalog.
// This is the ONLY assembly path that resolves HOME. Assembly passes the
// returned Config to permission.NewWorkspaceStore.
func interactivePermissionConfig(canonicalWorkspace string) (permission.Config, error) {
	path, err := defaultPermissionsPath(canonicalWorkspace)
	if err != nil {
		return permission.Config{}, err
	}
	return permission.Config{
		Path:           path,
		FamilyEligible: productFamilyEligibility(),
	}, nil
}

// headlessPermissionConfig builds the headless read-only permission-store Config
// from an explicit path. An empty path yields an empty rule set; a non-empty
// path must be absolute (fail closed). It NEVER searches HOME. Assembly passes
// the returned Config to permission.NewReadOnlyStore.
func headlessPermissionConfig(explicitPath string) (permission.Config, error) {
	if explicitPath != "" && !filepath.IsAbs(explicitPath) {
		return permission.Config{}, fmt.Errorf("coderig: headless permission-file path must be absolute, got %q", explicitPath)
	}
	return permission.Config{
		Path:           explicitPath,
		FamilyEligible: productFamilyEligibility(),
	}, nil
}
