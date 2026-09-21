package wsname

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv makes sure the override is not inherited from the test host.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvVar, "")
}

// requireNoGitAncestor skips the test if dir lives under a real git repo on this host.
func requireNoGitAncestor(t *testing.T, dir string) {
	t.Helper()

	resolved := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		resolved = r
	}
	if root, ok := mainRepoRoot(resolved); ok {
		t.Skipf("temp dir %q lives inside git repository %q on this host; "+
			"cannot exercise the no-git-anywhere case", resolved, root)
	}
}

// runGitT runs git in dir for test setup, failing the test on error.
func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=tether-test", "GIT_AUTHOR_EMAIL=tether-test@example.com",
		"GIT_COMMITTER_NAME=tether-test", "GIT_COMMITTER_EMAIL=tether-test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// initRepo creates a real git repository at dir with one commit, so
// git-common-dir/show-toplevel and worktree/submodule setup have a
// checked-out branch to work from.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "tether-test@example.com")
	runGitT(t, dir, "config", "user.name", "tether-test")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitT(t, dir, "add", "README")
	runGitT(t, dir, "commit", "-q", "-m", "init")
}

func TestResolvePlainRepo(t *testing.T) {
	clearEnv(t)
	root := filepath.Join(t.TempDir(), "storefront")
	initRepo(t, root)

	got, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", root, err)
	}
	suffix, ok := strings.CutPrefix(got, "storefront-")
	if !ok || len(suffix) != 6 {
		t.Fatalf("Resolve(%q) = %q, want %q plus a 6-hex-char suffix", root, got, "storefront-")
	}
}

func TestResolveSubdirectoryAgreesWithRoot(t *testing.T) {
	clearEnv(t)
	root := filepath.Join(t.TempDir(), "storefront")
	initRepo(t, root)
	sub := filepath.Join(root, "src", "ui")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	want, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve(root) returned error: %v", err)
	}
	got, err := Resolve(sub)
	if err != nil {
		t.Fatalf("Resolve(sub) returned error: %v", err)
	}
	if got != want {
		t.Fatalf("Resolve(%q) = %q, want %q (same as repo root)", sub, got, want)
	}
}

// TestResolveWorktreeSharesMainWorkspace is the core C3 fix: two worktrees of
// one repo must resolve to the same workspace even though their directory
// names differ.
func TestResolveWorktreeSharesMainWorkspace(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	main := filepath.Join(base, "storefront")
	initRepo(t, main)

	wt := filepath.Join(base, "storefront-feature-x")
	runGitT(t, main, "worktree", "add", "-q", "-b", "feature-x", wt)

	wantMain, err := Resolve(main)
	if err != nil {
		t.Fatalf("Resolve(main) returned error: %v", err)
	}
	gotWT, err := Resolve(wt)
	if err != nil {
		t.Fatalf("Resolve(worktree) returned error: %v", err)
	}
	if gotWT != wantMain {
		t.Fatalf("Resolve(worktree) = %q, Resolve(main) = %q; worktrees of one repo must share a workspace",
			gotWT, wantMain)
	}
	if !strings.HasPrefix(gotWT, "storefront-") {
		t.Fatalf("worktree resolved to %q, want the main repo's basename (storefront), not its own directory name", gotWT)
	}
}

// TestResolveSubmoduleIsDistinct guards against a submodule's .git file (which
// also points outside itself, like a worktree's) being mistaken for one.
func TestResolveSubmoduleIsDistinct(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	inner := filepath.Join(base, "widget")
	initRepo(t, inner)

	outer := filepath.Join(base, "app")
	initRepo(t, outer)
	runGitT(t, outer, "-c", "protocol.file.allow=always", "submodule", "add", inner, "sub")

	gotOuter, err := Resolve(outer)
	if err != nil {
		t.Fatalf("Resolve(outer) returned error: %v", err)
	}
	gotSub, err := Resolve(filepath.Join(outer, "sub"))
	if err != nil {
		t.Fatalf("Resolve(submodule) returned error: %v", err)
	}
	if gotSub == gotOuter {
		t.Fatalf("submodule resolved to the superproject's workspace %q; a submodule must keep its own identity", gotOuter)
	}
	// git checks a submodule out under its own directory name ("sub" here),
	// not the origin repo's name ("widget"): that checkout dir is its root.
	if !strings.HasPrefix(gotSub, "sub-") {
		t.Fatalf("Resolve(submodule) = %q, want prefix %q", gotSub, "sub-")
	}
}

// TestResolveNoRemoteHashesAbsolutePath guards the false-merge half of C3:
// same basename, different location, no remote to disambiguate by.
func TestResolveNoRemoteHashesAbsolutePath(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	a := filepath.Join(base, "group-a", "api")
	b := filepath.Join(base, "group-b", "api")
	initRepo(t, a)
	initRepo(t, b)

	gotA, err := Resolve(a)
	if err != nil {
		t.Fatalf("Resolve(a) returned error: %v", err)
	}
	gotB, err := Resolve(b)
	if err != nil {
		t.Fatalf("Resolve(b) returned error: %v", err)
	}
	if gotA == gotB {
		t.Fatalf("two remote-less repos both named %q at different paths resolved to the same workspace %q", "api", gotA)
	}
}

// TestResolveSameBasenameDifferentOriginDiffer is the other false-merge case:
// same basename, both have a remote, but the remotes differ.
func TestResolveSameBasenameDifferentOriginDiffer(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	a := filepath.Join(base, "team-a", "api")
	b := filepath.Join(base, "team-b", "api")
	initRepo(t, a)
	initRepo(t, b)
	runGitT(t, a, "remote", "add", "origin", "git@github.com:team-a/api.git")
	runGitT(t, b, "remote", "add", "origin", "git@github.com:team-b/api.git")

	gotA, err := Resolve(a)
	if err != nil {
		t.Fatalf("Resolve(a) returned error: %v", err)
	}
	gotB, err := Resolve(b)
	if err != nil {
		t.Fatalf("Resolve(b) returned error: %v", err)
	}
	if gotA == gotB {
		t.Fatalf("repos named %q with different origins both resolved to %q; false merge", "api", gotA)
	}
	if !strings.HasPrefix(gotA, "api-") || !strings.HasPrefix(gotB, "api-") {
		t.Fatalf("Resolve = %q / %q, want both prefixed %q", gotA, gotB, "api-")
	}
}

func TestResolveNoGitAnywhere(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	requireNoGitAncestor(t, root)

	deep := filepath.Join(root, "plain", "deep", "deeper")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{name: "leaf directory", cwd: deep, want: "deeper"},
		{name: "mid directory", cwd: filepath.Join(root, "plain", "deep"), want: "deep"},
		{name: "top directory", cwd: filepath.Join(root, "plain"), want: "plain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.cwd)
			if err != nil {
				t.Fatalf("Resolve(%q) returned error: %v", tc.cwd, err)
			}
			if got != tc.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

func TestResolveEnvOverride(t *testing.T) {
	dir := t.TempDir()
	const override = "my-workspace"
	t.Setenv(EnvVar, override)

	for _, cwd := range []string{"", ".", dir, filepath.Join(dir, "does", "not", "exist")} {
		got, err := Resolve(cwd)
		if err != nil {
			t.Fatalf("Resolve(%q) returned error: %v", cwd, err)
		}
		if got != override {
			t.Fatalf("Resolve(%q) = %q, want override %q", cwd, got, override)
		}
	}
}

func TestResolveEnvOverrideVerbatim(t *testing.T) {
	dir := t.TempDir()
	values := []string{"team alpha", "/looks/like/a/path", "  padded  ", "UPPER-case_Mixed.123"}

	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvVar, v)
			got, err := Resolve(dir)
			if err != nil {
				t.Fatalf("Resolve returned error: %v", err)
			}
			if got != v {
				t.Fatalf("Resolve = %q, want %q verbatim", got, v)
			}
		})
	}
}

func TestResolveEnvEmptyFallsThrough(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storefront")
	initRepo(t, root)
	t.Setenv(EnvVar, "")

	got, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", root, err)
	}
	if !strings.HasPrefix(got, "storefront-") {
		t.Fatalf("Resolve(%q) = %q, want prefix %q", root, got, "storefront-")
	}
}

func TestResolveRelativeMatchesAbsolute(t *testing.T) {
	clearEnv(t)
	root := filepath.Join(t.TempDir(), "storefront")
	initRepo(t, root)
	deep := filepath.Join(root, "src", "ui")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	want, err := Resolve(deep)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", deep, err)
	}

	t.Chdir(deep)
	for _, cwd := range []string{"", ".", "./", "../ui", filepath.Join("..", "..", "src", "ui")} {
		got, err := Resolve(cwd)
		if err != nil {
			t.Fatalf("Resolve(%q) returned error: %v", cwd, err)
		}
		if got != want {
			t.Fatalf("Resolve(%q) = %q, want %q (same as absolute form)", cwd, got, want)
		}
	}
}

func TestResolveFollowsSymlinks(t *testing.T) {
	clearEnv(t)
	base := t.TempDir()
	root := filepath.Join(base, "storefront")
	initRepo(t, root)
	realDir := filepath.Join(root, "src")
	if err := os.MkdirAll(realDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	link := filepath.Join(base, "link-to-src")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("cannot create symlinks on this host: %v", err)
	}

	gotReal, err := Resolve(realDir)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", realDir, err)
	}
	gotLink, err := Resolve(link)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", link, err)
	}
	if gotLink != gotReal {
		t.Fatalf("Resolve via symlink %q = %q, but via real path %q = %q; must agree", link, gotLink, realDir, gotReal)
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	clearEnv(t)
	root := filepath.Join(t.TempDir(), "storefront")
	initRepo(t, root)

	first, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve returned error: %v", err)
		}
		if got != first {
			t.Fatalf("Resolve = %q on call %d, want stable %q", got, i, first)
		}
	}
}
