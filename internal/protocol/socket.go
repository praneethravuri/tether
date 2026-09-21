package protocol

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// SocketPath resolves where the socket should live based on the hierarchy
// TETHER_SOCK, then $XDG_RUNTIME_DIR/tether/sock, then ~/.tether/sock.
func SocketPath() (string, error) {
	if sock := os.Getenv("TETHER_SOCK"); sock != "" {
		return sock, nil
	}

	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "tether", "sock"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not find home dir: %w", err)
	}

	return filepath.Join(home, ".tether", "sock"), nil
}

// DBPath resolves where the sqlite database should live: TETHER_DB if set,
// otherwise ~/.tether/tether.db. The ~/.tether directory is created with 0700
// so the message store is only readable by its owner.
func DBPath() (string, error) {
	if db := os.Getenv("TETHER_DB"); db != "" {
		return db, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not find home dir: %w", err)
	}

	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	return filepath.Join(dir, "tether.db"), nil
}

// LogPath resolves where the daemon's log lives: ~/.tether/daemon.log. This
// is the file every bug report starts with, so its location never varies
// with how the daemon was started.
func LogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not find home dir: %w", err)
	}

	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	return filepath.Join(dir, "daemon.log"), nil
}

// Listen creates and verifies a socket directory that group and other users
// cannot write to, removes a stale socket file, and binds inside it (see
// bindSocket for the platform-specific file permissions). The directory
// boundary keeps another user from replacing the socket between stale-path
// inspection and removal.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	if err := requireSafeSocketDir(dir); err != nil {
		return nil, err
	}

	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}

	listener, err := bindSocket(path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	return listener, nil
}

func requireSafeSocketDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket directory %s is not a directory", dir)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return fmt.Errorf("socket directory %s must not be writable by group or others, got %04o", dir, mode)
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket path %s exists but is not a Unix socket", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}
