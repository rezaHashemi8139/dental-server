// Package releases publishes new versions of the desktop app, for its built-in
// updater (Tauri's updater plugin), and says which versions must update before
// they may keep working (a forced update).
//
// The folder UPDATES_DIR holds what the desktop app's `pnpm build` writes to its
// build/update folder: the installer and release.json:
//
//	{
//	  "version": "0.2.0",          // the version on offer
//	  "minimumVersion": "0.2.0",   // older apps are locked until they update ("" = none)
//	  "notes": "...",              // shown to the user, optional
//	  "pubDate": "2026-11-24T10:00:00Z",
//	  "platforms": {
//	    "windows-x86_64": { "file": "<installer file name>", "signature": "<contents of its .sig file>" }
//	  }
//	}
//
// Copy a new release over the old one; it is picked up without a restart.
// minimumVersion can also be raised by hand at any time, to force an update later.
package releases

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileName is the release description in UPDATES_DIR.
const FileName = "release.json"

// Version is a "major.minor.patch" version, the only form the app uses.
type Version [3]uint64

// ParseVersion reads "1.2.3".
func ParseVersion(s string) (Version, error) {
	var v Version
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("version %q is not major.minor.patch", s)
	}
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return v, fmt.Errorf("version %q is not major.minor.patch", s)
		}
		v[i] = n
	}
	return v, nil
}

// Less reports whether v is older than other.
func (v Version) Less(other Version) bool {
	for i := range v {
		if v[i] != other[i] {
			return v[i] < other[i]
		}
	}
	return false
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// Platform is the installer for one platform, e.g. "windows-x86_64".
type Platform struct {
	File      string `json:"file"`
	Signature string `json:"signature"`
}

// Release is a checked release.json.
type Release struct {
	Version Version
	// Zero when no version is forced to update.
	MinimumVersion Version
	Notes          string
	PubDate        time.Time
	Platforms      map[string]Platform
}

// Requires reports whether an app of version v must update before working.
// An app that does not say its version counts as the oldest.
func (r *Release) Requires(v Version) bool {
	return r != nil && v.Less(r.MinimumVersion)
}

type releaseFile struct {
	Version        string              `json:"version"`
	MinimumVersion string              `json:"minimumVersion"`
	Notes          string              `json:"notes"`
	PubDate        string              `json:"pubDate"`
	Platforms      map[string]Platform `json:"platforms"`
}

// parse checks a release.json; the installers it names must be in dir.
func parse(dir string, content []byte) (*Release, error) {
	var f releaseFile
	if err := json.Unmarshal(content, &f); err != nil {
		return nil, err
	}
	version, err := ParseVersion(f.Version)
	if err != nil {
		return nil, err
	}
	r := &Release{Version: version, Notes: f.Notes, Platforms: f.Platforms}
	if f.MinimumVersion != "" {
		if r.MinimumVersion, err = ParseVersion(f.MinimumVersion); err != nil {
			return nil, fmt.Errorf("minimumVersion: %w", err)
		}
		if version.Less(r.MinimumVersion) {
			return nil, fmt.Errorf("minimumVersion %s is newer than version %s: nobody could update to it", r.MinimumVersion, version)
		}
	}
	if f.PubDate != "" {
		if r.PubDate, err = time.Parse(time.RFC3339, f.PubDate); err != nil {
			return nil, fmt.Errorf("pubDate: %w", err)
		}
	}
	if len(f.Platforms) == 0 {
		return nil, errors.New(`no "platforms"`)
	}
	for name, p := range f.Platforms {
		if p.File == "" || p.File != filepath.Base(p.File) || strings.HasPrefix(p.File, ".") {
			return nil, fmt.Errorf("platform %s: %q is not a file name", name, p.File)
		}
		if strings.TrimSpace(p.Signature) == "" {
			return nil, fmt.Errorf("platform %s: no signature", name)
		}
		if info, err := os.Stat(filepath.Join(dir, p.File)); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("platform %s: installer %s not found in %s", name, p.File, dir)
		}
	}
	return r, nil
}

// Store reads UPDATES_DIR/release.json, again whenever the file changes.
type Store struct {
	dir string

	mu      sync.Mutex
	current *Release
	// Of the file last read, to notice changes.
	modTime time.Time
	size    int64
}

// Open reads the release in dir, if any. A broken release.json is an error: it should never ship.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := s.reload(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, FileName), err)
	}
	return s, nil
}

// Dir is the folder the installers are in.
func (s *Store) Dir() string {
	return s.dir
}

// Current is the release on offer, or nil if there is none (or no store). A
// release.json broken after startup is logged and the last good release kept.
func (s *Store) Current() *Release {
	if s == nil {
		return nil
	}
	if err := s.reload(); err != nil {
		slog.Error("release not updated: keeping the previous one", "file", filepath.Join(s.dir, FileName), "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *Store) reload() error {
	path := filepath.Join(s.dir, FileName)
	info, err := os.Stat(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		s.current, s.modTime, s.size = nil, time.Time{}, 0
		return nil
	}
	if err != nil {
		return err
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	release, err := parse(s.dir, content)
	if err != nil {
		return err
	}
	s.current, s.modTime, s.size = release, info.ModTime(), info.Size()
	slog.Info("app release", "version", release.Version, "minimumVersion", release.MinimumVersion)
	return nil
}
