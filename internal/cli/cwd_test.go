package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattkje/clav/internal/project"
	"github.com/mattkje/clav/internal/state"
)

// runFrom drives clav as if the user's shell were sitting in cwd.
func (h *harness) runFrom(cwd string, args ...string) (string, error) {
	h.t.Helper()
	var out bytes.Buffer
	store, err := state.Open(h.home)
	if err != nil {
		h.t.Fatal(err)
	}
	app := &App{Out: &out, Err: &out, In: strings.NewReader(h.stdin), Store: store, Cwd: cwd}
	err = app.Run(context.Background(), args)
	return out.String(), err
}

func (h *harness) mustRunFrom(cwd string, args ...string) string {
	h.t.Helper()
	out, err := h.runFrom(cwd, args...)
	if err != nil {
		h.t.Fatalf("clav %s (from %s) failed: %v\n%s", strings.Join(args, " "), cwd, err, out)
	}
	return out
}

// TestHarnessNeverUsesTheProcessWorkingDirectory guards the mistake that once
// deleted this repository: a bare `clav park` in a test must resolve against the
// harness's scratch directory, never against wherever `go test` happens to run.
func TestHarnessNeverUsesTheProcessWorkingDirectory(t *testing.T) {
	h := newHarness(t)
	if h.shell == "" {
		t.Fatal("the harness must always set a working directory")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if isInside(h.shell, wd) {
		t.Fatalf("the harness working directory %s is inside the source tree %s", h.shell, wd)
	}
	if root, marker, found := project.FindRoot(h.shell); found {
		t.Fatalf("the harness working directory resolves to project root %s (via %s); "+
			"a bare park could delete it", root, marker)
	}
}

func TestBareParkFromProjectRoot(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")

	out := h.mustRunFrom(root, "park", "--verbose")
	if !strings.Contains(out, "sorta parked") {
		t.Errorf("bare park did not target the project:\n%s", out)
	}
	if !strings.Contains(out, "found .git") {
		t.Errorf("--verbose should say how the target was chosen:\n%s", out)
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Fatal("bare park did not park the project")
	}
	h.mustRun("restore", root)
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("the project did not come back")
	}
}

func TestBareParkFromSubdirectoryParksTheProject(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	deep := filepath.Join(root, "src")

	out := h.mustRunFrom(deep, "park")
	if !strings.Contains(out, "sorta parked") {
		t.Errorf("park from a subdirectory should target the project root:\n%s", out)
	}
	if exists(t, filepath.Join(root, ".git")) {
		t.Fatal("the repository root was not parked")
	}
	// The kept file inside the directory we ran from is still there.
	if !exists(t, filepath.Join(deep, "scratch.md")) {
		t.Error("a kept file below the subdirectory was deleted")
	}
}

func TestBareParkInAPlainDirectoryIsRefused(t *testing.T) {
	h := newHarness(t)
	// No .git, no go.mod, nothing: a plain directory of files.
	root := h.path("just-a-folder")
	mk(t, root)
	writeFile(t, filepath.Join(root, "notes.txt"), "notes\n", 0o644)

	_, err := h.runFrom(root, "park")
	if err == nil {
		t.Fatal("park should refuse a directory that is not a repository")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("unhelpful error: %v", err)
	}
	if !exists(t, filepath.Join(root, "notes.txt")) {
		t.Error("a refused park deleted files anyway")
	}
}

func TestBareParkFromANestedPackageIsRefused(t *testing.T) {
	h := newHarness(t)
	outer, _ := h.remoteProject("monorepo")
	inner := filepath.Join(outer, "packages", "widget")
	mk(t, inner)
	writeFile(t, filepath.Join(inner, "package.json"), "{}\n", 0o644)

	// The nearest marker is the inner package.json, but that directory is only
	// part of a repository — clav must say so instead of parking half of it.
	_, err := h.runFrom(inner, "park")
	if err == nil {
		t.Fatal("park should refuse a directory inside a repository")
	}
	if !strings.Contains(err.Error(), "root of one") {
		t.Errorf("error should point at the repository root: %v", err)
	}
	if !exists(t, filepath.Join(outer, ".git")) {
		t.Error("the enclosing repository should be untouched")
	}
}

func TestParkNotesWhenTheShellIsInTheDeletedDirectory(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo") // no remote, so the folder is archived and removed
	deep := filepath.Join(root, "src")

	out := h.mustRunFrom(deep, "park")
	if !strings.Contains(out, "your shell is in the deleted directory") {
		t.Errorf("expected a note about the now-missing cwd:\n%s", out)
	}
	if !strings.Contains(out, "cd ") {
		t.Errorf("the note should suggest where to go:\n%s", out)
	}

	// Parking a project you are not standing in must not print the note.
	other := h.localProject("elsewhere")
	out = h.mustRunFrom(h.shell, "park", other)
	if strings.Contains(out, "your shell is") {
		t.Errorf("unexpected cwd note:\n%s", out)
	}
}

func TestParkKeepsQuietWhenTheFolderSurvives(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	// The folder still exists afterwards (it holds the kept files), so there is
	// nothing to warn about even though the shell is inside it.
	out := h.mustRunFrom(root, "park")
	if strings.Contains(out, "your shell is") {
		t.Errorf("unexpected cwd note:\n%s", out)
	}
}

// TestWorkingDirIsResolvedBeforeDeletion pins the reason the note once went
// missing in real use: os.Getwd fails after park deletes the directory, so the
// working directory has to be resolved (and cached) up front.
func TestWorkingDirIsResolvedBeforeDeletion(t *testing.T) {
	app := &App{}
	first, err := app.workingDir()
	if err != nil {
		t.Fatal(err)
	}
	if app.Cwd != first {
		t.Errorf("workingDir did not cache its answer: Cwd = %q, want %q", app.Cwd, first)
	}
	second, err := app.workingDir()
	if err != nil || second != first {
		t.Errorf("workingDir = %q, %v on the second call; want the cached %q", second, err, first)
	}
}

func TestRestoreGoesToOriginalPathNotTheCurrentDirectory(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	before := fingerprint(t, root)
	h.mustRun("park", root)

	// Restore from a completely unrelated working directory.
	elsewhere := filepath.Join(h.base, "somewhere", "else")
	mk(t, elsewhere)
	h.mustRunFrom(elsewhere, "restore", root)

	assertSameTree(t, before, fingerprint(t, root))
	if exists(t, filepath.Join(elsewhere, "solo")) {
		t.Error("the project was restored relative to the working directory")
	}
}

// Parking from inside a subdirectory and restoring from somewhere else entirely
// is the workflow `clav park` with no path is for; the project must land back at
// its original path, not next to wherever you happened to be standing.
func TestParkFromInsideRestoreFromElsewhere(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	deep := filepath.Join(root, "src")

	h.mustRunFrom(deep, "park")
	if exists(t, filepath.Join(root, "main.go")) {
		t.Fatal("project not parked")
	}

	h.mustRunFrom(h.shell, "restore", root)
	if !exists(t, filepath.Join(root, "main.go")) || !exists(t, filepath.Join(root, "notes.txt")) {
		t.Error("the project did not come back at its original path")
	}
}

func TestBareParkRefusesTooManyArguments(t *testing.T) {
	h := newHarness(t)
	a, _ := h.remoteProject("a")
	b, _ := h.remoteProject("b")
	if _, err := h.runFrom(h.shell, "park", a, b); err == nil {
		t.Fatal("park with two paths should fail")
	}
}

func TestBareRestoreFromInsideTheParkedFolder(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	h.mustRun("park", root)
	// The folder is still there — it holds the kept files — so the shell is
	// standing in the parked project and that is what restore should mean.
	h.mustRunFrom(root, "restore")
	if !exists(t, filepath.Join(root, "main.go")) || !exists(t, filepath.Join(root, "notes.txt")) {
		t.Error("bare restore did not bring the project back")
	}
}

func TestBareRestoreFromASubdirectoryOfTheParkedFolder(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	h.mustRun("park", root)
	// src survived because it held a kept file.
	h.mustRunFrom(filepath.Join(root, "src"), "restore")
	if !exists(t, filepath.Join(root, "src", "app.go")) {
		t.Error("bare restore from a subdirectory did not restore the project")
	}
}

func TestBareRestoreSaysWhatToDoWhenNothingIsParkedHere(t *testing.T) {
	h := newHarness(t)
	_, err := h.runFrom(h.shell, "restore")
	if err == nil {
		t.Fatal("bare restore should fail where nothing is parked")
	}
	if !strings.Contains(err.Error(), "nothing parked") || !strings.Contains(err.Error(), "clav list") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestBareRestoreFromADirectoryParkKeptNothingOf(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo") // archived whole, so the folder is gone
	h.mustRun("park", root)
	if exists(t, root) {
		t.Fatal("the folder should have been removed")
	}
	// The shell is still standing in the deleted directory; that path is what
	// restore has to resolve against.
	h.mustRunFrom(root, "restore")
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("bare restore did not bring the archived project back")
	}
}
