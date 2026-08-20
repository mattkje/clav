package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpenCreatesLayout(t *testing.T) {
	s := open(t)
	for _, dir := range []string{s.ArchivesDir(), s.TempDir()} {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			t.Errorf("%s was not created: %v", dir, err)
		}
	}
}

func TestDefaultRootHonoursEnv(t *testing.T) {
	t.Setenv(EnvHome, "/tmp/somewhere/clav")
	got, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/somewhere/clav" {
		t.Errorf("DefaultRoot = %q", got)
	}

	t.Setenv(EnvHome, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err = DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".clav"); got != want {
		t.Errorf("DefaultRoot = %q, want %q", got, want)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	s := open(t)
	f, err := s.Load()
	if err != nil {
		t.Fatalf("missing state should not be an error: %v", err)
	}
	if len(f.Projects) != 0 {
		t.Errorf("expected no projects, got %d", len(f.Projects))
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	s := open(t)
	if err := os.WriteFile(s.StatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("corrupt state should be reported, not silently discarded")
	}
}

func TestLoadRejectsNewerSchema(t *testing.T) {
	s := open(t)
	if err := os.WriteFile(s.StatePath(), []byte(`{"version":999,"projects":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("a newer schema should be refused rather than misread")
	}
}

func sample(name, key string) Project {
	return Project{
		ID: key + "-001", Name: name, PathKey: key,
		OriginalPath: "/p/" + name, Archive: "archives/" + key + "-001.tar.zst",
		CreatedAt: time.Now().UTC().Truncate(time.Second), ArchiveSize: 10,
	}
}

func TestUpdateIsAtomicAndNoOpOnError(t *testing.T) {
	s := open(t)
	if err := s.Update(func(f *File) error {
		f.Upsert(sample("a", "aaa"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	wantErr := errFake{}
	if err := s.Update(func(f *File) error {
		f.Upsert(sample("b", "bbb"))
		return wantErr
	}); err != wantErr {
		t.Fatalf("Update error = %v, want the callback's error", err)
	}

	f, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Projects) != 1 || f.Projects[0].Name != "a" {
		t.Fatalf("a failed update modified the state: %+v", f.Projects)
	}

	// The file must always be valid JSON with no stray temp files left behind.
	raw, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var check File
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	entries, err := os.ReadDir(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 6 && e.Name()[:7] == ".state-" {
			t.Errorf("leftover temp state file: %s", e.Name())
		}
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake" }

func TestConcurrentUpdatesAllLand(t *testing.T) {
	s := open(t)
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a'+i%26)) + string(rune('0'+i/26)) + "-key"
			if err := s.Update(func(f *File) error {
				f.Upsert(sample("p", key))
				if f.Counters == nil {
					f.Counters = map[string]int{}
				}
				f.Counters["total"]++
				return nil
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	f, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if f.Counters["total"] != n {
		t.Errorf("counter = %d, want %d (lost updates)", f.Counters["total"], n)
	}
	if len(f.Projects) != n {
		t.Errorf("projects = %d, want %d", len(f.Projects), n)
	}
}

func TestFindUpsertDelete(t *testing.T) {
	f := &File{}
	f.Upsert(sample("a", "k1"))
	f.Upsert(sample("b", "k2"))
	if _, ok := f.Find("k1"); !ok {
		t.Error("Find missed an existing project")
	}
	updated := sample("a2", "k1")
	f.Upsert(updated)
	if len(f.Projects) != 2 {
		t.Errorf("Upsert duplicated an entry: %d", len(f.Projects))
	}
	if p, _ := f.Find("k1"); p.Name != "a2" {
		t.Error("Upsert did not replace in place")
	}
	if !f.Delete("k1") {
		t.Error("Delete reported nothing removed")
	}
	if f.Delete("k1") {
		t.Error("Delete reported a second removal")
	}
	if len(f.Projects) != 1 {
		t.Errorf("projects after delete = %d, want 1", len(f.Projects))
	}
}

func TestSortedNewestFirst(t *testing.T) {
	now := time.Now().UTC()
	f := &File{Projects: []Project{
		{Name: "old", PathKey: "1", CreatedAt: now.Add(-72 * time.Hour)},
		{Name: "new", PathKey: "2", CreatedAt: now},
		{Name: "mid", PathKey: "3", CreatedAt: now.Add(-time.Hour)},
	}}
	got := f.Sorted()
	if got[0].Name != "new" || got[1].Name != "mid" || got[2].Name != "old" {
		t.Errorf("Sorted order = %v %v %v", got[0].Name, got[1].Name, got[2].Name)
	}
	if f.Projects[0].Name != "old" {
		t.Error("Sorted mutated the underlying slice")
	}
}

func TestResolveArchivePath(t *testing.T) {
	s := open(t)
	if got, want := s.Resolve("archives/x.tar.zst"), filepath.Join(s.Root(), "archives", "x.tar.zst"); got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
	if got := s.Resolve("/absolute/x.tar.zst"); got != "/absolute/x.tar.zst" {
		t.Errorf("Resolve mangled an absolute path: %q", got)
	}
}
