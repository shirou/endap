// Package store owns the on-disk history log: where it lives, how records are
// appended to it, how it is read back, and how it is rewritten in place.
package store

import (
	"errors"
	"os"
	"path/filepath"
)

// File names inside the data directory. The suffixes are the single spelling of
// each convention: the path helpers below and the rewrite temporary in
// rewrite.go all derive from them rather than repeating the literals.
const (
	HistoryName = "history.jsonl"
	RejSuffix   = ".rej"
	LockSuffix  = ".lock"
	TmpSuffix   = ".tmp."

	// RejName is the rejected-lines file as it appears in messages to the user.
	RejName = HistoryName + RejSuffix
)

// Permissions. The log can hold anything the user typed, including whatever the
// ignore patterns failed to catch, so it is never group- or world-readable.
const (
	DirPerm  os.FileMode = 0o700
	FilePerm os.FileMode = 0o600
)

// DataDir returns the directory holding history.jsonl.
//
// ENDAP_DATA_DIR wins, then $XDG_DATA_HOME/endap, then ~/.local/share/endap.
// The XDG data directory is resolved by hand because Go 1.26 has
// os.UserConfigDir and os.UserCacheDir but no os.UserDataDir; ConfigDir below
// does use the standard library. The asymmetry is in the standard library, not
// in endap.
func DataDir() (string, error) {
	if d := os.Getenv("ENDAP_DATA_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "endap"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "endap"), nil
}

// ConfigDir returns the directory holding the config file.
func ConfigDir() (string, error) {
	if d := os.Getenv("ENDAP_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "endap"), nil
}

// ConfigPath returns the config file path. ENDAP_CONFIG overrides it entirely.
func ConfigPath() (string, error) {
	if p := os.Getenv("ENDAP_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config"), nil
}

// HistoryPath returns the path of the history log.
func HistoryPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, HistoryName), nil
}

// RejPath returns the path of the rejected-lines file for a history log.
func RejPath(history string) string { return history + RejSuffix }

// LockPath returns the path of the rewrite lock for a history log.
func LockPath(history string) string { return history + LockSuffix }

// EnsureDir creates dir with 0700 if it does not exist.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return err
	}
	return nil
}

// TmpFiles lists leftover rewrite temporaries next to the history log. A
// crashed compact leaves a full copy of the log behind, so doctor reports them
// and forget has to search them.
func TmpFiles(history string) ([]string, error) {
	dir := filepath.Dir(history)
	base := filepath.Base(history) + TmpSuffix
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); len(name) > len(base) && name[:len(base)] == base {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}
