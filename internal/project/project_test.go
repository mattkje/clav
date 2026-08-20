package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyDistinguishesSameNameDifferentPaths(t *testing.T) {
	a := KeyFor("/Users/example/Projects/foo")
	b := KeyFor("/Users/example/Other/foo")
	if a == b {
		t.Fatal("projects with the same directory name must not share an identity")
	}
	if a != KeyFor("/Users/example/Projects/foo") {
		t.Fatal("identity must be stable for the same path")
	}
	if len(a) != 16 {
		t.Fatalf("key length = %d, want 16", len(a))
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := Expand("~/Projects/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Projects", "x"); got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
	if _, err := Expand(""); err == nil {
		t.Error("Expand(\"\") should fail")
	}
}

func TestCanonicalWorksForMissingPaths(t *testing.T) {
	// A parked project's directory no longer exists, so restore must still be
	// able to canonicalise its original path.
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	got, err := Canonical(filepath.Join(link, "gone", "deeper"))
	if err != nil {
		t.Fatal(err)
	}
	realResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realResolved, "gone", "deeper"); got != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}
}

func TestCanonicalIsStableAcrossParkAndRestore(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "proj")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	whileThere, err := Canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	whileGone, err := Canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	if whileThere != whileGone {
		t.Errorf("identity changed once the directory was deleted: %q vs %q", whileThere, whileGone)
	}
}

func TestResolveRejections(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "a-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "a-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	clavHome := filepath.Join(base, "clav-home")
	if err := os.MkdirAll(filepath.Join(clavHome, "archives"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, path, want string
	}{
		{"missing", filepath.Join(base, "nope"), "no such directory"},
		{"file", file, "not a directory"},
		{"symlink", link, "symlink"},
		{"storage root", clavHome, "overlaps"},
		{"inside storage", filepath.Join(clavHome, "archives"), "overlaps"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Resolve(c.path, clavHome)
			if err == nil {
				t.Fatalf("Resolve(%q) succeeded, want error", c.path)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}

	if _, err := Resolve(dir, clavHome); err != nil {
		t.Errorf("Resolve on a plain directory failed: %v", err)
	}
}

func TestResolveRefusesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := Resolve(home); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("expected a refusal to park the home directory, got %v", err)
	}
}

func TestScanCountsEverything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	must(t, os.MkdirAll(filepath.Join(root, "sub", "deeper"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "empty"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, ".env"), []byte("A=1"), 0o600))
	must(t, os.WriteFile(filepath.Join(root, "sub", "f.txt"), []byte("12345"), 0o644))
	must(t, os.Symlink("f.txt", filepath.Join(root, "sub", "l")))

	st, err := Scan(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 {
		t.Errorf("files = %d, want 2", st.Files)
	}
	if st.Dirs != 4 { // root, sub, sub/deeper, empty
		t.Errorf("dirs = %d, want 4", st.Dirs)
	}
	if st.Symlinks != 1 {
		t.Errorf("symlinks = %d, want 1", st.Symlinks)
	}
	if st.Size != 8 {
		t.Errorf("size = %d, want 8", st.Size)
	}
	if st.Entries != 7 {
		t.Errorf("entries = %d, want 7", st.Entries)
	}
}

func TestScanHonoursFilter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	must(t, os.MkdirAll(filepath.Join(root, "node_modules", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "node_modules", "x", "a.js"), []byte("a"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644))

	st, err := Scan(root, NewPatterns([]string{"node_modules"}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 1 || st.Dirs != 1 {
		t.Errorf("scan with filter = %d files / %d dirs, want 1 / 1", st.Files, st.Dirs)
	}
}

func TestShortenUsesTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := Shorten(filepath.Join(home, "Projects", "x")); got != filepath.Join("~", "Projects", "x") {
		t.Errorf("Shorten = %q", got)
	}
	if got := Shorten("/elsewhere/x"); got != "/elsewhere/x" {
		t.Errorf("Shorten = %q, want unchanged", got)
	}
	if got := Shorten(home); got != "~" {
		t.Errorf("Shorten(home) = %q, want ~", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
