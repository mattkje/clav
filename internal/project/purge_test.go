package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsJunkMatchesRegenerableDirectories(t *testing.T) {
	junk := []string{"node_modules/", "node_modules/left-pad/index.js", "target/", "src/.venv/",
		"packages/web/dist/", "__pycache__/"}
	for _, p := range junk {
		if !IsJunk(p) {
			t.Errorf("IsJunk(%q) = false, want true", p)
		}
	}
	keep := []string{".env", "notes.txt", "src/app.go", "distribution/plan.md", ".idea/workspace.xml", ""}
	for _, p := range keep {
		if IsJunk(p) {
			t.Errorf("IsJunk(%q) = true, want false", p)
		}
	}
}

func TestRemoveDeletesListedPathsAndPrunesEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"main.go", "src/app.go", "src/notes.md", "node_modules/pad/index.js"} {
		mkfile(t, filepath.Join(root, p), "x")
	}

	got, err := Remove(root, []string{"main.go", "src/app.go", "node_modules/"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Files != 3 {
		t.Errorf("files = %d, want 3 (two listed files plus the one inside node_modules)", got.Files)
	}
	if got.Bytes != 3 {
		t.Errorf("bytes = %d, want 3", got.Bytes)
	}
	for _, gone := range []string{"main.go", "src/app.go", "node_modules"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); err == nil {
			t.Errorf("%s still exists", gone)
		}
	}
	// src still holds a file, so it stays; node_modules left nothing behind.
	if _, err := os.Stat(filepath.Join(root, "src", "notes.md")); err != nil {
		t.Errorf("an unlisted file was deleted: %v", err)
	}
}

func TestRemovePrunesDirectoriesThatOnlyHeldTrackedFiles(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, "a/b/c/deep.go"), "x")
	if _, err := Remove(root, []string{"a/b/c/deep.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
		t.Error("an emptied directory tree should be pruned")
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the project root itself must survive: %v", err)
	}
}

func TestRemoveRefusesPathsOutsideTheProject(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(filepath.Dir(root), "sibling.txt")
	mkfile(t, sibling, "precious")
	t.Cleanup(func() { _ = os.Remove(sibling) })

	for _, bad := range []string{"../sibling.txt", "/etc/hosts", ".", "/"} {
		if _, err := Remove(root, []string{bad}); err == nil {
			t.Errorf("Remove(%q) should have been refused", bad)
		}
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Error("a refused path was deleted anyway")
	}
}

func TestOverlayMergesIntoTheDirectoryThatIsAlreadyThere(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	// The clone.
	mkfile(t, filepath.Join(src, "main.go"), "repository version")
	mkfile(t, filepath.Join(src, "src", "app.go"), "app")
	mkfile(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main")
	// What park left behind.
	mkfile(t, filepath.Join(dst, "notes.txt"), "notes")
	mkfile(t, filepath.Join(dst, "src", "scratch.md"), "scratch")
	mkfile(t, filepath.Join(dst, "main.go"), "my version")

	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Overlay(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "main.go" {
		t.Errorf("conflicts = %v, want [main.go]", res.Conflicts)
	}
	if got := read(t, filepath.Join(dst, "main.go")); got != "repository version" {
		t.Errorf("the repository copy should win: %q", got)
	}
	if got := read(t, filepath.Join(dst, "main.go.clav-kept")); got != "my version" {
		t.Errorf("the kept copy should survive beside it: %q", got)
	}
	for path, want := range map[string]string{
		"notes.txt":      "notes",
		"src/scratch.md": "scratch",
		"src/app.go":     "app",
		".git/HEAD":      "ref: refs/heads/main",
	} {
		if got := read(t, filepath.Join(dst, filepath.FromSlash(path))); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if left, err := os.ReadDir(src); err != nil || len(left) != 0 {
		t.Errorf("the source should be empty afterwards: %v %v", left, err)
	}

	// The directory itself must not have been replaced: a shell standing in it
	// has to stay valid.
	after, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("Overlay replaced the destination directory instead of merging into it")
	}
}

func TestWipeEmptiesWithoutRemovingTheDirectory(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "a/b.txt"), "x")
	mkfile(t, filepath.Join(dir, "c.txt"), "x")
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Wipe(dir); err != nil {
		t.Fatal(err)
	}
	if !IsEmpty(dir) {
		t.Error("Wipe left something behind")
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("Wipe replaced the directory")
	}
}

func TestEntriesAndIsEmpty(t *testing.T) {
	root := t.TempDir()
	if files, bytes, err := Entries(root); err != nil || files != 0 || bytes != 0 {
		t.Errorf("Entries on an empty dir = %d, %d, %v", files, bytes, err)
	}
	if !IsEmpty(root) {
		t.Error("a fresh directory should be empty")
	}
	mkfile(t, filepath.Join(root, "a/b.txt"), "hello")
	files, bytes, err := Entries(root)
	if err != nil || files != 1 || bytes != 5 {
		t.Errorf("Entries = %d, %d, %v; want 1, 5, nil", files, bytes, err)
	}
	if IsEmpty(root) {
		t.Error("a directory with content is not empty")
	}
}

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}
