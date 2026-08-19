// Package wsname resolves a working directory to a tether workspace name,
// derived from the repo's main root (worktrees included) so agents in the
// same repo agree and same-named repos on different remotes do not collide.
package wsname

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnvVar overrides workspace-name detection when set to a non-empty value.
const EnvVar = "TETHER_WORKSPACE"

// gitEntry is the basename of git's shared repository directory.
const gitEntry = ".git"

// Resolve returns the workspace name for cwd: TETHER_WORKSPACE if set,
// otherwise an identity derived from the git repo root, falling back to
// cwd's own basename when cwd is not inside a git repo.
func Resolve(cwd string) (string, error) {
	if ws := os.Getenv(EnvVar); ws != "" {
		return ws, nil
	}

	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("wsname: absolute path for %q: %w", cwd, err)
	}

	// Best effort: symlink resolution failure falls back to the plain path.
	dir := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		dir = resolved
	}

	if root, ok := mainRepoRoot(dir); ok {
		return identity(root), nil
	}

	return filepath.Base(dir), nil
}

// mainRepoRoot resolves dir to the root of its main repository. A linked
// worktree resolves to the repo it was created from, so agents in either
// share a workspace. A submodule keeps its own root, since its
// --git-common-dir lives under modules/ rather than ending in .git.
func mainRepoRoot(dir string) (string, bool) {
	common, err := gitCommonDir(dir)
	if err != nil {
		return "", false
	}
	if filepath.Base(common) == gitEntry {
		return filepath.Dir(common), true
	}

	root, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	return root, true
}

// gitCommonDir returns the absolute path of dir's shared git directory,
// falling back to the pre-2.31 form (--path-format is unsupported there,
// and the result may be relative to dir) on older git.
func gitCommonDir(dir string) (string, error) {
	if out, err := runGit(dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		return out, nil
	}

	out, err := runGit(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return filepath.Clean(out), nil
}

// identity names a repo root by basename plus a short hash of its origin
// URL, so two repos sharing a basename don't collide. Remote-less repos
// hash the absolute root path instead.
func identity(root string) string {
	hashInput := root
	if url, err := runGit(root, "remote", "get-url", "origin"); err == nil && url != "" {
		hashInput = url
	}
	sum := sha256.Sum256([]byte(hashInput))
	return fmt.Sprintf("%s-%x", filepath.Base(root), sum[:3])
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
