//go:build unix

// POSIX-only: asserts 0700/0600 mode bits, which Windows does not enforce.

package protocol

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tempDir returns a short path under /tmp because unix socket paths are capped
// at ~104 bytes on macOS, and t.TempDir() can exceed that.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tether-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSocketPath_NoHome(t *testing.T) {
	t.Setenv("TETHER_SOCK", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", "")

	if _, err := SocketPath(); err == nil {
		t.Fatal("SocketPath() with no HOME: want error, got nil")
	}
}

func TestSocketPath_EnvOverride(t *testing.T) {
	expected := "/tmp/custom.sock"
	t.Setenv("TETHER_SOCK", expected) // t.Setenv safely cleans up after the test

	got, err := SocketPath()
	if err != nil {
		t.Fatalf("SocketPath() error = %v", err)
	}

	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestSocketPath_Precedence(t *testing.T) {
	t.Run("TETHER_SOCK wins over XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("TETHER_SOCK", "/tmp/explicit.sock")
		t.Setenv("XDG_RUNTIME_DIR", "/tmp/xdg")
		t.Setenv("HOME", "/tmp/home")

		got, err := SocketPath()
		if err != nil {
			t.Fatal(err)
		}
		if got != "/tmp/explicit.sock" {
			t.Errorf("got %q, want %q", got, "/tmp/explicit.sock")
		}
	})

	t.Run("XDG_RUNTIME_DIR wins over home", func(t *testing.T) {
		t.Setenv("TETHER_SOCK", "")
		t.Setenv("XDG_RUNTIME_DIR", "/tmp/xdg")
		t.Setenv("HOME", "/tmp/home")

		got, err := SocketPath()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join("/tmp/xdg", "tether", "sock")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("falls back to home", func(t *testing.T) {
		home := tempDir(t)
		t.Setenv("TETHER_SOCK", "")
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("HOME", home)

		got, err := SocketPath()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(home, ".tether", "sock")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestDBPath_EnvOverride(t *testing.T) {
	t.Setenv("TETHER_DB", "/tmp/custom.db")
	t.Setenv("HOME", tempDir(t))

	got, err := DBPath()
	if err != nil {
		t.Fatalf("DBPath() error = %v", err)
	}
	if got != "/tmp/custom.db" {
		t.Errorf("got %q, want %q", got, "/tmp/custom.db")
	}
}

func TestDBPath_DefaultUnderHome(t *testing.T) {
	home := tempDir(t)
	t.Setenv("TETHER_DB", "")
	t.Setenv("HOME", home)

	got, err := DBPath()
	if err != nil {
		t.Fatalf("DBPath() error = %v", err)
	}

	want := filepath.Join(home, ".tether", "tether.db")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// The parent directory must exist and be 0700.
	info, err := os.Stat(filepath.Join(home, ".tether"))
	if err != nil {
		t.Fatalf("~/.tether was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("~/.tether is not a directory")
	}
	if info.Mode().Perm() != 0700 {
		t.Errorf("dir permissions got %04o, want 0700", info.Mode().Perm())
	}

	// Calling it again on an existing directory must still succeed.
	if _, err := DBPath(); err != nil {
		t.Fatalf("second DBPath() failed: %v", err)
	}
}

func TestDBPath_NoHome(t *testing.T) {
	t.Setenv("TETHER_DB", "")
	t.Setenv("HOME", "")

	if _, err := DBPath(); err == nil {
		t.Fatal("DBPath() with no HOME: want error, got nil")
	}
}

func TestDBPath_ParentIsAFile(t *testing.T) {
	home := tempDir(t)
	blocker := filepath.Join(home, ".tether")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	t.Setenv("TETHER_DB", "")
	t.Setenv("HOME", home)

	if _, err := DBPath(); err == nil {
		t.Fatal("DBPath() with a file where ~/.tether belongs: want error, got nil")
	}
}

func TestLogPath_DefaultUnderHome(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)

	got, err := LogPath()
	if err != nil {
		t.Fatalf("LogPath() error = %v", err)
	}

	want := filepath.Join(home, ".tether", "daemon.log")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLogPath_NoHome(t *testing.T) {
	t.Setenv("HOME", "")

	if _, err := LogPath(); err == nil {
		t.Fatal("LogPath() with no HOME: want error, got nil")
	}
}

func TestListen_ParentIsAFile(t *testing.T) {
	dir := tempDir(t)
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}

	if _, err := Listen(filepath.Join(blocker, "sock")); err == nil {
		t.Fatal("Listen() with a file where the parent directory belongs: want error, got nil")
	}
}

func TestListen_PreservesNonSocketPath(t *testing.T) {
	sockPath := filepath.Join(tempDir(t), "sock")
	const contents = "do not remove\n"
	if err := os.WriteFile(sockPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}

	listener, err := Listen(sockPath)
	if err == nil {
		_ = listener.Close()
		t.Fatal("Listen() over a regular file: want error, got nil")
	}

	// #nosec G304 -- sockPath is a test-owned file created in tempDir.
	raw, err := os.ReadFile(sockPath)
	if err != nil {
		t.Fatalf("read regular file after Listen: %v", err)
	}
	if string(raw) != contents {
		t.Fatalf("regular file contents = %q, want %q", raw, contents)
	}
}

func TestListen_AcceptsNonWritableSocketDirectory(t *testing.T) {
	dir := tempDir(t)
	// #nosec G302 -- this test intentionally verifies a non-writable 0755 directory.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("make socket directory non-writable: %v", err)
	}

	listener, err := Listen(filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatalf("Listen() in a non-writable directory: %v", err)
	}
	defer func() { _ = listener.Close() }()
}

func TestListen_RejectsWritableSocketDirectory(t *testing.T) {
	dir := tempDir(t)
	// #nosec G302 -- this test intentionally verifies rejection of a group-writable directory.
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatalf("make socket directory group-writable: %v", err)
	}

	if _, err := Listen(filepath.Join(dir, "sock")); err == nil {
		t.Fatal("Listen() in a writable directory: want error, got nil")
	}
}

// unix's sun_path caps at roughly 104 bytes; a longer path makes bind(2) itself fail.
func TestListen_PathTooLong(t *testing.T) {
	dir := tempDir(t)
	long := filepath.Join(dir, strings.Repeat("x", 200), "sock")

	if _, err := Listen(long); err == nil {
		t.Fatal("Listen() with an oversized socket path: want error, got nil")
	}
}

func TestListen_Permissions(t *testing.T) {
	// Listen must create the directory itself, so point at a nested path.
	dir := filepath.Join(tempDir(t), "run")
	sockPath := filepath.Join(dir, "sock")

	listener, err := Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen() failed: %v", err)
	}
	defer func() { _ = listener.Close() }()

	// 1. Verify directory is 0700
	dirStat, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirStat.Mode().Perm() != 0700 {
		t.Errorf("dir permissions got %04o, want 0700", dirStat.Mode().Perm())
	}

	// 2. Verify socket is 0600
	sockStat, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if sockStat.Mode().Perm() != 0600 {
		t.Errorf("socket permissions got %04o, want 0600", sockStat.Mode().Perm())
	}
	if sockStat.Mode()&os.ModeSocket == 0 {
		t.Errorf("expected a socket, got mode %v", sockStat.Mode())
	}
}

// TestListen_Dialable proves the listener actually serves newline-delimited
// JSON over the socket, which is the whole contract of this package.
func TestListen_Dialable(t *testing.T) {
	sockPath := filepath.Join(tempDir(t), "run", "sock")

	listener, err := Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen() failed: %v", err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		dec := json.NewDecoder(bufio.NewReader(conn))
		enc := json.NewEncoder(conn)
		for {
			var req Request
			if err := dec.Decode(&req); err != nil {
				return
			}
			if err := enc.Encode(OK(req.ID, WaitResult{Pending: 3})); err != nil {
				return
			}
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	// Two requests on the same connection: connections are reused.
	for i := 0; i < 2; i++ {
		if err := enc.Encode(Request{ID: "hb", Method: MethodWait}); err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}

		var resp Response
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if resp.Error != nil {
			t.Fatalf("unexpected error response: %v", resp.Error)
		}
		if resp.ID != "hb" {
			t.Errorf("ID got %q, want %q", resp.ID, "hb")
		}

		var result WaitResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if result.Pending != 3 {
			t.Errorf("Pending got %d, want 3", result.Pending)
		}
	}

	_ = conn.Close()
	_ = listener.Close()
	<-done
}

// TestListen_RemovesStaleSocket covers daemon restart after an unclean exit:
// the socket file is still on disk with no process behind it, and binding must
// succeed anyway.
func TestListen_RemovesStaleSocket(t *testing.T) {
	sockPath := filepath.Join(tempDir(t), "run", "sock")

	first, err := Listen(sockPath)
	if err != nil {
		t.Fatalf("first Listen() failed: %v", err)
	}
	// Simulate a crashed daemon: leave the socket file behind on close.
	if ul, ok := first.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	} else {
		t.Fatalf("Listen returned %T, want *net.UnixListener", first)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first listener: %v", err)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("stale socket file should still exist: %v", err)
	}

	second, err := Listen(sockPath)
	if err != nil {
		t.Fatalf("second Listen() over a stale socket failed: %v", err)
	}
	defer func() { _ = second.Close() }()

	// The new listener owns the path and is dialable.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial after rebind failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("socket permissions got %04o, want 0600", info.Mode().Perm())
	}
}
