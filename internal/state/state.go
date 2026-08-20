// Package state persists clav's metadata in ~/.clav/state.json.
//
// Every mutation goes through Update, which takes an exclusive lock, reloads
// from disk, applies the change and writes the file atomically. An interrupted
// operation can therefore never leave a torn or partially written state file.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SchemaVersion is bumped when the on-disk format changes incompatibly.
const SchemaVersion = 2

// Kinds of parked project.
const (
	// KindRemote means the tracked content was deleted and is restored by
	// cloning the project's remote again.
	KindRemote = "remote"
	// KindArchive means the whole directory was archived, which is what clav
	// does for a repository that has no remote.
	KindArchive = "archive"
)

// EnvHome overrides the storage root. Used by tests and by users who keep
// clav's data somewhere other than ~/.clav.
const EnvHome = "CLAV_HOME"

// Project is one parked project.
type Project struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	OriginalPath string    `json:"original_path"`
	PathKey      string    `json:"path_key"`
	Archive      string    `json:"archive"`
	Compression  string    `json:"compression"`
	CreatedAt    time.Time `json:"created_at"`
	OriginalSize int64     `json:"original_size"`
	ArchiveSize  int64     `json:"archive_size"`
	SHA256       string    `json:"sha256"`
	EntryCount   int       `json:"entry_count"`
	TreeSHA256   string    `json:"tree_sha256"`
	Cycle        int       `json:"cycle"`
	Excludes     []string  `json:"excludes,omitempty"`
	ClavVersion  string    `json:"clav_version,omitempty"`

	// Remote-kind fields. Together they are everything needed to fetch the
	// project's content back from its origin.
	RemoteName string `json:"remote_name,omitempty"`
	RemoteURL  string `json:"remote_url,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Submodules bool   `json:"submodules,omitempty"`
	// KeptFiles counts the untracked files left behind in the project folder.
	KeptFiles int `json:"kept_files,omitempty"`
	// FreedBytes is how much disk parking reclaimed.
	FreedBytes int64 `json:"freed_bytes,omitempty"`
}

// KindOf reports a project's kind, defaulting to archive for state written
// before kinds existed.
func (p Project) KindOf() string {
	if p.Kind == "" {
		return KindArchive
	}
	return p.Kind
}

// File is the whole state document.
type File struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
	// Counters records how many times each path has been parked. It survives
	// restore and remove so that a new archive for a previously parked path can
	// never reuse an old archive's name.
	Counters map[string]int `json:"counters,omitempty"`
}

// Find returns the parked project for a path key.
func (f *File) Find(pathKey string) (Project, bool) {
	for _, p := range f.Projects {
		if p.PathKey == pathKey {
			return p, true
		}
	}
	return Project{}, false
}

// Upsert inserts or replaces a project by path key.
func (f *File) Upsert(p Project) {
	for i := range f.Projects {
		if f.Projects[i].PathKey == p.PathKey {
			f.Projects[i] = p
			return
		}
	}
	f.Projects = append(f.Projects, p)
}

// Delete removes a project by path key, reporting whether it was present.
func (f *File) Delete(pathKey string) bool {
	for i := range f.Projects {
		if f.Projects[i].PathKey == pathKey {
			f.Projects = append(f.Projects[:i], f.Projects[i+1:]...)
			return true
		}
	}
	return false
}

// Sorted returns projects newest first.
func (f *File) Sorted() []Project {
	out := make([]Project, len(f.Projects))
	copy(out, f.Projects)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Name < out[j].Name
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Store is clav's storage root.
type Store struct {
	root string
}

// DefaultRoot is the storage root honouring CLAV_HOME.
func DefaultRoot() (string, error) {
	if v := os.Getenv(EnvHome); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", err
		}
		return filepath.Clean(abs), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".clav"), nil
}

// Open prepares the storage root, creating it if necessary.
func Open(root string) (*Store, error) {
	if root == "" {
		var err error
		if root, err = DefaultRoot(); err != nil {
			return nil, err
		}
	}
	s := &Store{root: root}
	if err := os.MkdirAll(s.ArchivesDir(), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", root, err)
	}
	if err := os.MkdirAll(s.TempDir(), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", s.TempDir(), err)
	}
	return s, nil
}

// Root is the storage directory (~/.clav by default).
func (s *Store) Root() string { return s.root }

// ArchivesDir holds the compressed project archives.
func (s *Store) ArchivesDir() string { return filepath.Join(s.root, "archives") }

// TempDir holds in-progress archives. It lives on the same filesystem as
// ArchivesDir so that completed archives can be moved into place atomically.
func (s *Store) TempDir() string { return filepath.Join(s.root, "tmp") }

// StatePath is the metadata file.
func (s *Store) StatePath() string { return filepath.Join(s.root, "state.json") }

// Resolve turns a stored relative archive reference into an absolute path.
func (s *Store) Resolve(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(s.root, rel)
}

// Load reads the state file. A missing file is an empty state, not an error.
func (s *Store) Load() (*File, error) {
	data, err := os.ReadFile(s.StatePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &File{Version: SchemaVersion}, nil
		}
		return nil, fmt.Errorf("cannot read %s: %w", s.StatePath(), err)
	}
	if len(data) == 0 {
		return &File{Version: SchemaVersion}, nil
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", s.StatePath(), err)
	}
	if f.Version == 0 {
		f.Version = SchemaVersion
	}
	if f.Version > SchemaVersion {
		return nil, fmt.Errorf("%s was written by a newer version of clav (schema %d)", s.StatePath(), f.Version)
	}
	return &f, nil
}

// Update applies fn to the state under an exclusive lock and writes the result
// atomically. If fn returns an error, nothing is written.
func (s *Store) Update(fn func(*File) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	f, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	f.Version = SchemaVersion
	return s.save(f)
}

// save writes the state file atomically: temp file in the same directory,
// fsync, rename, then fsync the directory.
func (s *Store) save(f *File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.root, ".state-*.json")
	if err != nil {
		return fmt.Errorf("cannot write state: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("cannot write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("cannot flush state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.StatePath()); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cannot commit state: %w", err)
	}
	return fsyncDir(s.root)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil // best effort; not fatal on platforms that refuse this
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}
