package archive

import (
	"archive/tar"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// fixture builds a project directory that exercises everything clav promises to
// preserve: tracked and untracked content, ignored directories, dotfiles,
// executables, empty directories, symlinks, hard links and awkward names.
func fixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "my project")
	mkdirAll(t, root)

	write(t, filepath.Join(root, "README.md"), "hello", 0o644)
	write(t, filepath.Join(root, ".env"), "SECRET=1", 0o600)
	write(t, filepath.Join(root, ".env.local"), "LOCAL=2", 0o644)
	write(t, filepath.Join(root, ".hidden"), "", 0o644)
	write(t, filepath.Join(root, ".gitignore"), "node_modules/\n", 0o644)
	write(t, filepath.Join(root, "script.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(root, "read-only.txt"), "locked", 0o444)

	mkdirAll(t, filepath.Join(root, "node_modules", "pkg"))
	write(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "ignored", 0o644)

	mkdirAll(t, filepath.Join(root, ".git", "objects"))
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)

	mkdirAll(t, filepath.Join(root, "empty"))
	mkdirAll(t, filepath.Join(root, "empty", "deeper"))
	mkdirAll(t, filepath.Join(root, "dir with spaces"))
	write(t, filepath.Join(root, "dir with spaces", "file with spaces.txt"), "spaced", 0o644)

	mkdirAll(t, filepath.Join(root, "restricted"))
	write(t, filepath.Join(root, "restricted", "inside.txt"), "inside", 0o644)
	if err := os.Chmod(filepath.Join(root, "restricted"), 0o500); err != nil {
		t.Fatal(err)
	}
	// Restore write permission at the end — on every copy of the tree, extracted
	// ones included — or t.TempDir cannot clean up and the test fails after it
	// has already passed.
	relaxOnCleanup(t)

	symlink(t, "README.md", filepath.Join(root, "link-relative"))
	symlink(t, "/nonexistent/outside", filepath.Join(root, "link-absolute-dangling"))

	write(t, filepath.Join(root, "hard-a.txt"), "linked content", 0o644)
	if err := os.Link(filepath.Join(root, "hard-a.txt"), filepath.Join(root, "hard-b.txt")); err != nil {
		t.Fatal(err)
	}

	// A distinctive modification time so preservation is testable.
	stamp := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "README.md"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return root
}

// relaxOnCleanup makes every directory under the test's temporary root
// writable again once the test ends.
func relaxOnCleanup(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Dir(root), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// entry records everything about one tree node that a restore must reproduce.
type entry struct {
	mode    os.FileMode
	size    int64
	link    string
	modTime time.Time
	content string
}

func roundTrip(t *testing.T, root string) (string, *Manifest) {
	t.Helper()
	store := t.TempDir()
	dest := filepath.Join(store, "a.tar.zst")
	m, err := Create(context.Background(), CreateOptions{Root: root, Dest: dest, TempDir: store})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := Verify(context.Background(), VerifyOptions{
		Path: dest, SHA256: m.SHA256, Entries: m.Entries, TreeSHA256: m.TreeSHA256,
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if _, err := Extract(context.Background(), ExtractOptions{Path: dest, Dest: out}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return out, m
}

func TestRoundTripPreservesEverything(t *testing.T) {
	root := fixture(t)
	before := lstatSnapshot(t, root)
	out, _ := roundTrip(t, root)
	after := lstatSnapshot(t, out)

	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("%s missing after restore", name)
			continue
		}
		if got.mode != want.mode {
			t.Errorf("%s: mode %v, want %v", name, got.mode, want.mode)
		}
		if got.link != want.link {
			t.Errorf("%s: symlink target %q, want %q", name, got.link, want.link)
		}
		if got.content != want.content {
			t.Errorf("%s: content %q, want %q", name, got.content, want.content)
		}
		if got.size != want.size {
			t.Errorf("%s: size %d, want %d", name, got.size, want.size)
		}
		if want.mode&os.ModeSymlink == 0 {
			if d := got.modTime.Sub(want.modTime); d > time.Second || d < -time.Second {
				t.Errorf("%s: mtime %v, want %v", name, got.modTime, want.modTime)
			}
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s appeared after restore but was not in the original", name)
		}
	}
	// Spot-check the promises the spec calls out by name.
	for _, must := range []string{".env", ".env.local", ".hidden", ".gitignore",
		".git/HEAD", "node_modules/pkg/index.js", "empty/deeper",
		"dir with spaces/file with spaces.txt", "link-absolute-dangling"} {
		if _, ok := after[must]; !ok {
			t.Errorf("%s was not restored", must)
		}
	}
}

// lstatSnapshot walks without following symlinks.
func lstatSnapshot(t *testing.T, root string) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		e := entry{mode: info.Mode(), modTime: info.ModTime()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if e.link, err = os.Readlink(p); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			e.size = info.Size()
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			e.content = string(b)
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoundTripPreservesHardLinks(t *testing.T) {
	root := fixture(t)
	out, _ := roundTrip(t, root)

	a, err := os.Stat(filepath.Join(out, "hard-a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(out, "hard-b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Error("hard link was restored as a separate file")
	}
}

func TestRoundTripEmptyProject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "empty-project")
	mkdirAll(t, root)
	out, m := roundTrip(t, root)
	if m.Entries != 1 {
		t.Errorf("entries = %d, want 1 (the root itself)", m.Entries)
	}
	fi, err := os.Stat(out)
	if err != nil || !fi.IsDir() {
		t.Fatalf("restored root is not a directory: %v", err)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	root := fixture(t)
	store := t.TempDir()
	dest := filepath.Join(store, "a.tar.zst")
	m, err := Create(context.Background(), CreateOptions{Root: root, Dest: dest, TempDir: store})
	if err != nil {
		t.Fatal(err)
	}

	// Flip a byte in the middle of the compressed stream.
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Verify(context.Background(), VerifyOptions{
		Path: dest, SHA256: m.SHA256, Entries: m.Entries, TreeSHA256: m.TreeSHA256,
	}); err == nil {
		t.Fatal("Verify accepted a corrupted archive")
	}
}

func TestVerifyDetectsTruncation(t *testing.T) {
	root := fixture(t)
	store := t.TempDir()
	dest := filepath.Join(store, "a.tar.zst")
	m, err := Create(context.Background(), CreateOptions{Root: root, Dest: dest, TempDir: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(dest, m.Size/2); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), VerifyOptions{Path: dest, SHA256: m.SHA256}); err == nil {
		t.Fatal("Verify accepted a truncated archive")
	}
}

func TestVerifyDetectsChecksumMismatch(t *testing.T) {
	root := fixture(t)
	store := t.TempDir()
	dest := filepath.Join(store, "a.tar.zst")
	if _, err := Create(context.Background(), CreateOptions{Root: root, Dest: dest, TempDir: store}); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(context.Background(), VerifyOptions{Path: dest, SHA256: strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected a checksum error, got %v", err)
	}
}

func TestVerifyDetectsStructuralChange(t *testing.T) {
	root := fixture(t)
	store := t.TempDir()
	dest := filepath.Join(store, "a.tar.zst")
	m, err := Create(context.Background(), CreateOptions{Root: root, Dest: dest, TempDir: store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(context.Background(), VerifyOptions{Path: dest, TreeSHA256: strings.Repeat("a", 64)})
	if err == nil {
		t.Fatal("Verify accepted an archive with the wrong structure")
	}
	if _, err := Verify(context.Background(), VerifyOptions{Path: dest, Entries: m.Entries + 1}); err == nil {
		t.Fatal("Verify accepted an archive with the wrong entry count")
	}
}

func TestCreateLeavesNoTempFileOnFailure(t *testing.T) {
	root := fixture(t)
	store := t.TempDir()
	dest := filepath.Join(store, "a.tar.zst")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Create(ctx, CreateOptions{Root: root, Dest: dest, TempDir: store}); err == nil {
		t.Fatal("Create succeeded with a cancelled context")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a partial archive was left at the destination")
	}
	names, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		t.Errorf("leftover file in temp dir: %s", n.Name())
	}
	// The source project must be untouched.
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		t.Errorf("source project was disturbed: %v", err)
	}
}

func TestCreateFailsWhenTempDirMissing(t *testing.T) {
	root := fixture(t)
	_, err := Create(context.Background(), CreateOptions{
		Root:    root,
		Dest:    filepath.Join(t.TempDir(), "a.tar.zst"),
		TempDir: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err == nil {
		t.Fatal("Create succeeded with an unusable temp dir")
	}
}

func TestExtractRefusesExistingDestination(t *testing.T) {
	root := fixture(t)
	store := t.TempDir()
	dest := filepath.Join(store, "a.tar.zst")
	if _, err := Create(context.Background(), CreateOptions{Root: root, Dest: dest, TempDir: store}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "exists")
	mkdirAll(t, out)
	if _, err := Extract(context.Background(), ExtractOptions{Path: dest, Dest: out}); err == nil {
		t.Fatal("Extract overwrote an existing directory")
	}
}

// writeHostileArchive builds an archive by hand containing an entry that tries
// to escape the destination.
func writeHostileArchive(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, "hostile.tar.zst")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	body := []byte("owned")
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Typeflag: tar.TypeReg, Mode: 0o644,
		Size: int64(len(body)), Format: tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	for _, c := range []func() error{tw.Close, enc.Close} {
		if err := c(); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{"../escape.txt", "/etc/escape.txt", "a/../../escape.txt"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := writeHostileArchive(t, dir, name)
			if _, err := Verify(context.Background(), VerifyOptions{Path: p}); err == nil {
				t.Error("Verify accepted an archive with an escaping path")
			}
			out := filepath.Join(dir, "out")
			if _, err := Extract(context.Background(), ExtractOptions{Path: p, Dest: out}); err == nil {
				t.Error("Extract accepted an archive with an escaping path")
			}
			if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
				t.Error("a file was written outside the destination")
			}
		})
	}
}

func TestVerifyRejectsArchiveWithoutRoot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "no-root.tar.zst")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	if err := tw.WriteHeader(&tar.Header{Name: "loose.txt", Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []func() error{tw.Close, enc.Close, f.Close} {
		if err := c(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Verify(context.Background(), VerifyOptions{Path: p}); err == nil {
		t.Fatal("Verify accepted an archive with no project root")
	}
}

// stubFilter excludes anything whose path contains the given substring.
type stubFilter struct{ contains string }

func (s stubFilter) Exclude(rel string, _ bool) bool { return strings.Contains(rel, s.contains) }

func TestFilterIsHonouredButUnusedByDefault(t *testing.T) {
	root := fixture(t)
	store := t.TempDir()

	all := filepath.Join(store, "all.tar.zst")
	if _, err := Create(context.Background(), CreateOptions{Root: root, Dest: all, TempDir: store}); err != nil {
		t.Fatal(err)
	}
	out, _ := roundTrip(t, root)
	if _, err := os.Stat(filepath.Join(out, "node_modules", "pkg", "index.js")); err != nil {
		t.Fatalf("default behaviour must archive everything: %v", err)
	}

	filtered := filepath.Join(store, "filtered.tar.zst")
	if _, err := Create(context.Background(), CreateOptions{
		Root: root, Dest: filtered, TempDir: store, Filter: stubFilter{contains: "node_modules"},
	}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "filtered-out")
	if _, err := Extract(context.Background(), ExtractOptions{Path: filtered, Dest: dst}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Error("filter did not exclude node_modules")
	}
	if _, err := os.Stat(filepath.Join(dst, ".env")); err != nil {
		t.Errorf("filter excluded too much: %v", err)
	}
}

func TestCheckName(t *testing.T) {
	bad := []string{"", "/abs", "../up", "a/../../up", "a/.."}
	for _, name := range bad {
		if err := checkName(name); err == nil {
			t.Errorf("checkName(%q) = nil, want error", name)
		}
	}
	good := []string{".", "./", "a", "a/b", "a b/c d.txt", ".env", "..hidden", "a/..b"}
	for _, name := range good {
		if err := checkName(name); err != nil {
			t.Errorf("checkName(%q) = %v, want nil", name, err)
		}
	}
}
