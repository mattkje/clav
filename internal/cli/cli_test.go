package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clav/internal/state"
)

// harness is a clav installation with its own storage root, driven exactly the
// way a user drives the binary.
type harness struct {
	t    *testing.T
	home string // CLAV_HOME
	base string // where projects live
	// shell is the working directory commands run from. It is always set, and
	// always a scratch directory: a bare `clav park` in a test must never be
	// able to resolve to the real source tree. (It once did, and deleted it.)
	shell string
	stdin string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	h := &harness{
		t:     t,
		home:  filepath.Join(root, "clav-home"),
		base:  filepath.Join(root, "Projects"),
		shell: filepath.Join(root, "shell"),
	}
	for _, dir := range []string{h.base, h.shell} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(state.EnvHome, h.home)
	// Every git invocation — the test's and clav's own — must be independent of
	// whoever is running the suite.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "clav test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "clav test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	return h
}

// run executes one clav command and returns its combined output.
func (h *harness) run(args ...string) (string, error) {
	h.t.Helper()
	var out bytes.Buffer
	store, err := state.Open(h.home)
	if err != nil {
		h.t.Fatal(err)
	}
	app := &App{Out: &out, Err: &out, In: strings.NewReader(h.stdin), Store: store, Cwd: h.shell}
	err = app.Run(context.Background(), args)
	return out.String(), err
}

// mustRun fails the test if the command does not succeed.
func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	out, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("clav %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (h *harness) path(name string) string { return filepath.Join(h.base, name) }

// contents builds the files every test project has: tracked source, an ignored
// dependency directory, an ignored .env and an untracked note.
func (h *harness) contents(root string) {
	h.t.Helper()
	mk(h.t, filepath.Join(root, "src"))
	writeFile(h.t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	writeFile(h.t, filepath.Join(root, "src", "app.go"), "package src\n", 0o644)
	writeFile(h.t, filepath.Join(root, ".gitignore"), "node_modules/\n.env\nsecrets.txt\n", 0o644)
}

// extras are the files that only exist locally: they must survive a park.
func (h *harness) extras(root string) {
	h.t.Helper()
	writeFile(h.t, filepath.Join(root, ".env"), "SECRET=shhh\n", 0o600)
	writeFile(h.t, filepath.Join(root, "notes.txt"), "my notes\n", 0o644)
	writeFile(h.t, filepath.Join(root, "secrets.txt"), "ignored but precious\n", 0o644)
	writeFile(h.t, filepath.Join(root, "src", "scratch.md"), "untracked, nested\n", 0o644)
	mk(h.t, filepath.Join(root, "node_modules", "left-pad"))
	writeFile(h.t, filepath.Join(root, "node_modules", "left-pad", "index.js"), "module.exports=1\n", 0o644)
}

// remoteProject is a repository with a bare remote that has everything pushed,
// plus local-only files. This is the case park is built for.
func (h *harness) remoteProject(name string) (root, remote string) {
	h.t.Helper()
	remote = filepath.Join(h.base, "remotes", name+".git")
	mk(h.t, filepath.Dir(remote))
	gitAt(h.t, h.base, "init", "--bare", "-q", remote)

	root = h.path(name)
	mk(h.t, root)
	gitAt(h.t, root, "init", "-q", "-b", "main")
	h.contents(root)
	gitAt(h.t, root, "add", ".")
	gitAt(h.t, root, "commit", "-qm", "initial")
	gitAt(h.t, root, "remote", "add", "origin", remote)
	gitAt(h.t, root, "push", "-q", "-u", "origin", "main")
	h.extras(root)
	return root, remote
}

// localProject is a repository with no remote: nowhere to clone it back from,
// so clav archives it whole.
func (h *harness) localProject(name string) string {
	h.t.Helper()
	root := h.path(name)
	mk(h.t, root)
	gitAt(h.t, root, "init", "-q", "-b", "main")
	h.contents(root)
	gitAt(h.t, root, "add", ".")
	gitAt(h.t, root, "commit", "-qm", "initial")
	h.extras(root)
	return root
}

func gitAt(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitAtOld runs git with a commit date far enough in the past that a sweep
// treats the project as idle.
func gitAtOld(t *testing.T, dir string, args ...string) {
	t.Helper()
	old := time.Now().Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+old, "GIT_COMMITTER_DATE="+old)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mk(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}

// fingerprint captures a tree so before/after can be compared exactly.
func fingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
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
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, rerr := os.Readlink(p)
			if rerr != nil {
				return rerr
			}
			out[rel] = "symlink " + target
		case info.IsDir():
			out[rel] = "dir " + info.Mode().Perm().String()
		default:
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[rel] = "file " + info.Mode().Perm().String() + " " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSameTree(t *testing.T, want, got map[string]string) {
	t.Helper()
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s is missing after the round trip", name)
			continue
		}
		if g != w {
			t.Errorf("%s changed:\n  before %s\n  after  %s", name, w, g)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s appeared out of nowhere", name)
		}
	}
}

// --- park against a remote -------------------------------------------------

func TestParkDeletesTrackedContentAndKeepsEverythingElse(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")

	out := h.mustRun("park", root)
	if !strings.Contains(out, "parked") {
		t.Errorf("unexpected park output:\n%s", out)
	}

	for _, gone := range []string{"main.go", ".git", ".gitignore",
		filepath.Join("src", "app.go"), "node_modules"} {
		if exists(t, filepath.Join(root, gone)) {
			t.Errorf("%s should have been deleted", gone)
		}
	}
	for _, kept := range []string{".env", "notes.txt", "secrets.txt",
		filepath.Join("src", "scratch.md")} {
		if !exists(t, filepath.Join(root, kept)) {
			t.Errorf("%s was not kept", kept)
		}
	}
	if got := mustRead(t, filepath.Join(root, ".env")); got != "SECRET=shhh\n" {
		t.Errorf(".env content changed: %q", got)
	}
}

func TestRestoreClonesBackAndPutsKeptFilesWithIt(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	h.mustRun("park", root)

	out := h.mustRun("restore", root)
	if !strings.Contains(out, "restored") {
		t.Errorf("unexpected restore output:\n%s", out)
	}
	for _, want := range []string{"main.go", ".git", ".gitignore",
		filepath.Join("src", "app.go"), ".env", "notes.txt", "secrets.txt",
		filepath.Join("src", "scratch.md")} {
		if !exists(t, filepath.Join(root, want)) {
			t.Errorf("%s is missing after restore", want)
		}
	}
	// A working repository, on the branch it was parked from, with the kept
	// files showing up exactly as they did before: untracked or ignored.
	if branch := strings.TrimSpace(gitOut(t, root, "rev-parse", "--abbrev-ref", "HEAD")); branch != "main" {
		t.Errorf("restored on branch %q, want main", branch)
	}
	status := gitOut(t, root, "status", "--porcelain")
	if !strings.Contains(status, "?? notes.txt") {
		t.Errorf("kept file is not untracked in the restored repo:\n%s", status)
	}
	if strings.Contains(status, ".env") {
		t.Errorf(".env should still be ignored:\n%s", status)
	}
	if out := h.mustRun("list"); strings.Contains(out, "sorta") {
		t.Errorf("restore should release the entry:\n%s", out)
	}
}

func TestParkRemovesTheFolderWhenNothingIsKept(t *testing.T) {
	h := newHarness(t)
	remote := filepath.Join(h.base, "remotes", "clean.git")
	mk(t, filepath.Dir(remote))
	gitAt(t, h.base, "init", "--bare", "-q", remote)
	root := h.path("clean")
	mk(t, root)
	gitAt(t, root, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	gitAt(t, root, "add", ".")
	gitAt(t, root, "commit", "-qm", "initial")
	gitAt(t, root, "remote", "add", "origin", remote)
	gitAt(t, root, "push", "-q", "-u", "origin", "main")

	h.mustRun("park", root)
	if exists(t, root) {
		t.Error("an empty project folder should be removed")
	}
	h.mustRun("restore", root)
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("restore did not bring the project back")
	}
}

func TestParkRefusesUncommittedChanges(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("dirty")
	writeFile(t, filepath.Join(root, "main.go"), "package main // edited\n", 0o644)

	_, err := h.run("park", root)
	if err == nil {
		t.Fatal("park should refuse a dirty working copy")
	}
	if !strings.Contains(err.Error(), "uncommitted") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the problem and the remedy: %v", err)
	}
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Fatal("a refused park deleted files anyway")
	}

	h.mustRun("park", root, "--force")
	if exists(t, filepath.Join(root, "main.go")) {
		t.Error("--force should park anyway")
	}
}

func TestParkRefusesUnpushedCommits(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("ahead")
	writeFile(t, filepath.Join(root, "extra.go"), "package main\n", 0o644)
	gitAt(t, root, "add", "extra.go")
	gitAt(t, root, "commit", "-qm", "local only")

	_, err := h.run("park", root)
	if err == nil {
		t.Fatal("park should refuse unpushed commits")
	}
	if !strings.Contains(err.Error(), "commit") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the problem and the remedy: %v", err)
	}
	if !exists(t, filepath.Join(root, "extra.go")) {
		t.Fatal("a refused park deleted files anyway")
	}

	// An unpushed commit on a branch with no upstream counts too.
	gitAt(t, root, "reset", "-q", "--hard", "origin/main")
	gitAt(t, root, "checkout", "-q", "-b", "side")
	writeFile(t, filepath.Join(root, "side.go"), "package main\n", 0o644)
	gitAt(t, root, "add", "side.go")
	gitAt(t, root, "commit", "-qm", "side work")
	gitAt(t, root, "checkout", "-q", "main")
	if _, err := h.run("park", root); err == nil {
		t.Fatal("park should refuse a branch the remote does not have")
	}
}

func TestParkRefusesAStash(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("stashy")
	writeFile(t, filepath.Join(root, "main.go"), "package main // wip\n", 0o644)
	gitAt(t, root, "stash", "push", "-q")

	_, err := h.run("park", root)
	if err == nil {
		t.Fatal("park should refuse a repository with a stash")
	}
	if !strings.Contains(err.Error(), "stash") {
		t.Errorf("error should mention the stash: %v", err)
	}
}

func TestParkRefusesAnUnreachableRemote(t *testing.T) {
	h := newHarness(t)
	root, remote := h.remoteProject("orphan")
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}

	_, err := h.run("park", root)
	if err == nil {
		t.Fatal("park should refuse when the remote cannot be reached")
	}
	if !strings.Contains(err.Error(), "cannot reach") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the problem and the remedy: %v", err)
	}
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("a refused park deleted files anyway")
	}
}

func TestParkRefusesANonGitDirectory(t *testing.T) {
	h := newHarness(t)
	root := h.path("plain")
	mk(t, root)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)

	_, err := h.run("park", root)
	if err == nil {
		t.Fatal("park should refuse a directory that is not a repository")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("unhelpful error: %v", err)
	}
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("a refused park deleted files anyway")
	}
}

func TestParkRefusesASubdirectoryOfARepository(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("whole")
	_, err := h.run("park", filepath.Join(root, "src"))
	if err == nil {
		t.Fatal("park should refuse half a repository")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("error should point at the repository root: %v", err)
	}
}

func TestKeepIgnoredLeavesDependencyDirectories(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("keepdeps")
	h.mustRun("park", root, "--keep-ignored")

	if !exists(t, filepath.Join(root, "node_modules", "left-pad", "index.js")) {
		t.Error("--keep-ignored should leave node_modules alone")
	}
	if exists(t, filepath.Join(root, "main.go")) {
		t.Error("tracked files should still be deleted")
	}
}

func TestRestoreKeepsBothCopiesWhenAKeptFileCollides(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("collide")
	h.mustRun("park", root)
	// A file the remote also has now appears among the kept files.
	writeFile(t, filepath.Join(root, "main.go"), "local version\n", 0o644)

	out := h.mustRun("restore", root)
	if !strings.Contains(out, "clav-kept") {
		t.Errorf("a collision should be reported:\n%s", out)
	}
	if got := mustRead(t, filepath.Join(root, "main.go")); got != "package main\n" {
		t.Errorf("the repository's file should win: %q", got)
	}
	if got := mustRead(t, filepath.Join(root, "main.go.clav-kept")); got != "local version\n" {
		t.Errorf("the kept file should survive beside it: %q", got)
	}
}

func TestRestoreRefusesAnExistingRepository(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("occupied")
	h.mustRun("park", root)
	gitAt(t, root, "init", "-q")
	writeFile(t, filepath.Join(root, "unrelated.txt"), "do not lose me\n", 0o644)

	_, err := h.run("restore", root)
	if err == nil {
		t.Fatal("restore replaced a repository without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the remedy: %v", err)
	}
	if !exists(t, filepath.Join(root, "unrelated.txt")) {
		t.Error("a refused restore disturbed the directory")
	}
	h.mustRun("restore", root, "--force")
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("--force did not restore the project")
	}
}

func TestParkRefusesAlreadyParked(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	h.mustRun("park", root)
	writeFile(t, filepath.Join(root, "new.txt"), "new\n", 0o644)

	_, err := h.run("park", root)
	if err == nil {
		t.Fatal("park should refuse a location that is already parked")
	}
	if !strings.Contains(err.Error(), "already parked") {
		t.Errorf("unexpected error: %v", err)
	}
	if !exists(t, filepath.Join(root, "new.txt")) {
		t.Error("a refused park deleted the directory anyway")
	}
}

func TestSameNameDifferentPathsCoexist(t *testing.T) {
	h := newHarness(t)
	base := h.base
	h.base = filepath.Join(base, "one")
	mk(t, h.base)
	a, _ := h.remoteProject("foo")
	h.base = filepath.Join(base, "two")
	mk(t, h.base)
	b, _ := h.remoteProject("foo")
	h.base = base

	h.mustRun("park", a)
	h.mustRun("park", b)
	if out := h.mustRun("list"); strings.Count(out, "foo") < 2 {
		t.Errorf("both projects should be listed:\n%s", out)
	}

	h.mustRun("restore", a)
	if !exists(t, filepath.Join(a, ".git")) {
		t.Error("the first project was not restored")
	}
	if exists(t, filepath.Join(b, ".git")) {
		t.Error("restoring one project resurrected the other")
	}
}

func TestRepeatedParkRestoreCycles(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("cycler")

	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(root, "cycle.txt"), strings.Repeat("x", i+1), 0o644)
		h.mustRun("park", root)
		if exists(t, filepath.Join(root, ".git")) {
			t.Fatalf("cycle %d: tracked content was not deleted", i)
		}
		h.mustRun("restore", root)
		if !exists(t, filepath.Join(root, "main.go")) {
			t.Fatalf("cycle %d: project was not restored", i)
		}
		if got := mustRead(t, filepath.Join(root, "cycle.txt")); got != strings.Repeat("x", i+1) {
			t.Fatalf("cycle %d: kept file changed: %q", i, got)
		}
	}
	f, err := loadState(t, h.home)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Projects) != 0 {
		t.Errorf("state should be empty, got %+v", f.Projects)
	}
}

func TestKeepLeavesTheProjectListed(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("keeper")
	h.mustRun("park", root)
	h.mustRun("restore", root, "--keep")

	if out := h.mustRun("list"); !strings.Contains(out, "keeper") {
		t.Errorf("--keep should leave the project listed:\n%s", out)
	}
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("the project was not restored")
	}
}

// --- park with no remote (archive fallback) --------------------------------

func TestLocalOnlyRepositoryIsArchivedWholeAndRestoredExactly(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	before := fingerprint(t, root)

	out := h.mustRun("park", root)
	if !strings.Contains(out, "archived") {
		t.Errorf("park should say why it archived:\n%s", out)
	}
	if exists(t, root) {
		t.Fatal("park did not remove the original directory")
	}

	h.mustRun("restore", root)
	assertSameTree(t, before, fingerprint(t, root))
	if status := gitOut(t, root, "status", "--porcelain"); !strings.Contains(status, "?? notes.txt") {
		t.Errorf("the restored repository lost its untracked files:\n%s", status)
	}
}

func TestArchivedProjectWithSpacesInItsPath(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("my project (v2)")
	before := fingerprint(t, root)
	h.mustRun("park", root)
	h.mustRun("restore", root)
	assertSameTree(t, before, fingerprint(t, root))
}

func TestRestoreRefusesExistingDestinationForAnArchive(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	h.mustRun("park", root)

	mk(t, root)
	writeFile(t, filepath.Join(root, "unrelated.txt"), "do not lose me\n", 0o644)

	_, err := h.run("restore", root)
	if err == nil {
		t.Fatal("restore overwrote an existing directory without --force")
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the problem and the remedy, got: %v", err)
	}
	if !exists(t, filepath.Join(root, "unrelated.txt")) {
		t.Error("the existing directory was disturbed by a refused restore")
	}
	if out := h.mustRun("list"); !strings.Contains(out, "solo") {
		t.Errorf("the project should still be parked:\n%s", out)
	}
}

func TestRestoreForceReplaces(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	before := fingerprint(t, root)
	h.mustRun("park", root)

	mk(t, root)
	writeFile(t, filepath.Join(root, "unrelated.txt"), "replaced\n", 0o644)

	h.mustRun("restore", root, "--force")
	assertSameTree(t, before, fingerprint(t, root))

	entries, err := os.ReadDir(h.base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".clav-") {
			t.Errorf("restore left a working directory behind: %s", e.Name())
		}
	}
}

func TestRestoreRefusesCorruptArchive(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	h.mustRun("park", root)

	archives, err := os.ReadDir(filepath.Join(h.home, "archives"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("expected one archive: %v", err)
	}
	p := filepath.Join(h.home, "archives", archives[0].Name())
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.run("restore", root); err == nil {
		t.Fatal("restore accepted a corrupted archive")
	}
	if exists(t, root) {
		t.Error("a refused restore created the destination anyway")
	}
	entries, err := os.ReadDir(h.base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".clav-") {
			t.Errorf("a failed restore left %s behind", e.Name())
		}
	}
	if out := h.mustRun("list"); !strings.Contains(out, "solo") {
		t.Errorf("the project should still be listed after a failed restore:\n%s", out)
	}
}

func TestCorruptArchiveDoesNotDestroyExistingDirectoryEvenWithForce(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	h.mustRun("park", root)

	archives, _ := os.ReadDir(filepath.Join(h.home, "archives"))
	p := filepath.Join(h.home, "archives", archives[0].Name())
	if err := os.Truncate(p, 32); err != nil {
		t.Fatal(err)
	}

	mk(t, root)
	writeFile(t, filepath.Join(root, "precious.txt"), "keep me\n", 0o644)

	if _, err := h.run("restore", root, "--force"); err == nil {
		t.Fatal("restore --force accepted a corrupt archive")
	}
	if got := mustRead(t, filepath.Join(root, "precious.txt")); got != "keep me\n" {
		t.Errorf("--force destroyed the existing directory before verifying: %q", got)
	}
}

func TestArchiveParkFailureLeavesEverythingUntouched(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	before := fingerprint(t, root)

	// Break the staging area so archive creation cannot start.
	store, err := state.Open(h.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(store.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.TempDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	app := &App{Out: &out, Err: &out, In: strings.NewReader(""), Store: store, Cwd: h.shell}
	if err := app.Run(context.Background(), []string{"park", root}); err == nil {
		t.Fatal("park should have failed")
	}

	assertSameTree(t, before, fingerprint(t, root))
	archives, err := os.ReadDir(store.ArchivesDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 0 {
		t.Errorf("a failed park left archives behind: %d", len(archives))
	}
	// Repair the staging area so the state file can be opened again.
	if err := os.Remove(store.TempDir()); err != nil {
		t.Fatal(err)
	}
	f, err := loadState(t, h.home)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Projects) != 0 {
		t.Errorf("a failed park recorded metadata: %+v", f.Projects)
	}
}

func TestArchiveParkInterruptedLeavesEverythingUntouched(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	before := fingerprint(t, root)

	store, err := state.Open(h.home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // as if the user pressed Ctrl-C immediately

	var out bytes.Buffer
	app := &App{Out: &out, Err: &out, In: strings.NewReader(""), Store: store, Cwd: h.shell}
	err = app.Run(ctx, []string{"park", root})
	if err == nil {
		t.Fatal("an interrupted park should fail")
	}

	assertSameTree(t, before, fingerprint(t, root))
	for _, dir := range []string{store.ArchivesDir(), store.TempDir()} {
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(entries) != 0 {
			t.Errorf("%s is not clean after an interrupted park: %d entries", dir, len(entries))
		}
	}
}

// --- the rest of the CLI ---------------------------------------------------

func TestRemoveRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	h.mustRun("park", root)

	// Empty input (or anything that is not an explicit yes) means no.
	h.stdin = "\n"
	out := h.mustRun("remove", root)
	if !strings.Contains(out, "Remove solo? [y/N]") {
		t.Errorf("expected a confirmation prompt:\n%s", out)
	}
	if !strings.Contains(out, "Cancelled") {
		t.Errorf("declining should be explicit:\n%s", out)
	}
	if out := h.mustRun("list"); !strings.Contains(out, "solo") {
		t.Error("a declined remove deleted the project")
	}

	h.stdin = "n\n"
	h.mustRun("remove", root)
	if out := h.mustRun("list"); !strings.Contains(out, "solo") {
		t.Error("answering n deleted the project")
	}

	h.stdin = "y\n"
	h.mustRun("remove", root)
	if out := h.mustRun("list"); strings.Contains(out, "solo") {
		t.Errorf("confirmed remove did not delete the project:\n%s", out)
	}
	archives, err := os.ReadDir(filepath.Join(h.home, "archives"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 0 {
		t.Errorf("remove left the archive on disk: %d files", len(archives))
	}
}

func TestRemoveOfARemoteParkDeletesNothing(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	h.mustRun("park", root)

	out := h.mustRun("remove", root, "--force")
	if !strings.Contains(out, "forgotten") {
		t.Errorf("remove should say the code is untouched:\n%s", out)
	}
	if !exists(t, filepath.Join(root, "notes.txt")) {
		t.Error("remove deleted the kept files")
	}
	if out := h.mustRun("list"); strings.Contains(out, "sorta") {
		t.Error("the entry should be gone")
	}
}

func TestListAndInspect(t *testing.T) {
	h := newHarness(t)
	root, remote := h.remoteProject("sorta")

	if out := h.mustRun("list"); !strings.Contains(out, "no parked projects") {
		t.Errorf("empty list output:\n%s", out)
	}
	h.mustRun("park", root)

	out := h.mustRun("list")
	for _, want := range []string{"NAME", "FREED", "PARKED", "LOCATION", "sorta", "just now"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	out = h.mustRun("inspect", root)
	for _, want := range []string{"Project", "Path", "Parked", "Remote", "Commit", "Freed", remote} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

func TestInspectAnArchivedProject(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	h.mustRun("park", root)

	out := h.mustRun("inspect", root)
	for _, want := range []string{"Project", "Kind", "archive", "Size", "Archive"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
	if out := h.mustRun("inspect", root, "--verify"); !strings.Contains(out, "verified") {
		t.Errorf("inspect --verify output:\n%s", out)
	}
}

func TestCommandsOnUnknownProject(t *testing.T) {
	h := newHarness(t)
	missing := h.path("never-parked")
	for _, args := range [][]string{{"restore", missing}, {"inspect", missing}, {"remove", missing, "--force"}} {
		_, err := h.run(args...)
		if err == nil {
			t.Errorf("clav %s should fail for an unparked project", strings.Join(args, " "))
			continue
		}
		if !strings.Contains(err.Error(), "no parked project") {
			t.Errorf("unhelpful error for %v: %v", args, err)
		}
	}
}

func TestParkValidatesItsArgument(t *testing.T) {
	h := newHarness(t)
	file := h.path("a-file.txt")
	writeFile(t, file, "x", 0o644)

	cases := [][]string{
		{"park", h.path("nope")},
		{"park", file},
		{"park", h.path("a"), h.path("b")},
		{"nonsense"},
	}
	for _, args := range cases {
		if _, err := h.run(args...); err == nil {
			t.Errorf("clav %s should fail", strings.Join(args, " "))
		}
	}
}

func TestNormalOutputIsOneLine(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("quiet")

	out := h.mustRun("park", root)
	if lines := countLines(out); lines != 1 {
		t.Errorf("park printed %d lines, want 1:\n%s", lines, out)
	}
	out = h.mustRun("restore", root)
	if lines := countLines(out); lines != 1 {
		t.Errorf("restore printed %d lines, want 1:\n%s", lines, out)
	}
}

func countLines(s string) int {
	n := 0
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

func TestVerboseShowsEachStep(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("loud")
	out := h.mustRun("park", root, "--verbose")
	for _, want := range []string{"Checking for local work", "Asking the remote", "Deleting tracked content"} {
		if !strings.Contains(out, want) {
			t.Errorf("--verbose output missing %q:\n%s", want, out)
		}
	}
	h.mustRun("restore", root, "--verbose")
}

func TestHelpAndVersion(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("--help")
	for _, want := range []string{"clav park", "clav restore", "clav list", "clav inspect", "clav remove"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
	if out := h.mustRun("--version"); !strings.Contains(out, Version) {
		t.Errorf("version output: %s", out)
	}
}

func TestStateStaysValidJSONThroughout(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	steps := [][]string{{"park", root}, {"list"}, {"inspect", root}, {"restore", root}}
	for _, args := range steps {
		h.mustRun(args...)
		if _, err := loadState(t, h.home); err != nil {
			t.Fatalf("state unreadable after %v: %v", args, err)
		}
	}
}

func TestSizesAndDatesAreHumanReadable(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	out := h.mustRun("park", root)
	if !strings.Contains(out, "freed") {
		t.Errorf("park should report what it freed:\n%s", out)
	}
	if !strings.Contains(out, " KB") && !strings.Contains(out, " B") && !strings.Contains(out, " MB") {
		t.Errorf("sizes should be human readable:\n%s", out)
	}

	// Backdate the record so the relative date is not "just now".
	store, err := state.Open(h.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(f *state.File) error {
		f.Projects[0].CreatedAt = time.Now().Add(-50 * 24 * time.Hour).UTC()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out := h.mustRun("list"); !strings.Contains(out, "months ago") && !strings.Contains(out, "month ago") {
		t.Errorf("expected a relative date:\n%s", out)
	}
}

func loadState(t *testing.T, home string) (*state.File, error) {
	t.Helper()
	s, err := state.Open(home)
	if err != nil {
		return nil, err
	}
	return s.Load()
}

func TestForceSaysWhatItIsDestroying(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("forced")
	writeFile(t, filepath.Join(root, "main.go"), "package main // edited\n", 0o644)
	gitAt(t, root, "stash", "push", "-q")
	writeFile(t, filepath.Join(root, "extra.go"), "package main\n", 0o644)
	gitAt(t, root, "add", "extra.go")
	gitAt(t, root, "commit", "-qm", "never pushed")

	out := h.mustRun("park", root, "--force")
	for _, want := range []string{"stash entry", "1 commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("--force should warn about %q:\n%s", want, out)
		}
	}
}

func TestRestoreSurvivesABranchTheRemoteNeverHad(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("branchy")
	gitAt(t, root, "checkout", "-q", "-b", "local-only")

	h.mustRun("park", root, "--force")
	out := h.mustRun("restore", root)
	if !strings.Contains(out, "not on origin any more") {
		t.Errorf("restore should say the branch is gone:\n%s", out)
	}
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Fatal("the project should still be restored")
	}
	if branch := strings.TrimSpace(gitOut(t, root, "rev-parse", "--abbrev-ref", "HEAD")); branch != "main" {
		t.Errorf("restored on %q, want the default branch", branch)
	}
	if !exists(t, filepath.Join(root, "notes.txt")) {
		t.Error("kept files should still come back")
	}
}

// TestRestoreKeepsTheFolderItRestoresInto pins the bug that made the shell
// unusable: restore used to rename the project folder aside and swap a new one
// in, which left the terminal standing in a directory clav then deleted.
func TestRestoreKeepsTheFolderItRestoresInto(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	h.mustRun("park", root)

	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	h.mustRun("restore", root)
	after, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("restore replaced the project directory instead of restoring into it")
	}
	entries, err := os.ReadDir(h.base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".clav-") {
			t.Errorf("restore left %s behind", e.Name())
		}
	}
	// And the whole cycle works again from the same directory.
	h.mustRunFrom(root, "park")
	h.mustRunFrom(root, "restore")
	if !exists(t, filepath.Join(root, "main.go")) || !exists(t, filepath.Join(root, "notes.txt")) {
		t.Error("a second park/restore cycle from inside the folder failed")
	}
}

func TestForcedRestoreAlsoKeepsTheFolder(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")
	h.mustRun("park", root)
	// Someone put a different repository at that path.
	gitAt(t, root, "init", "-q")

	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	h.mustRun("restore", root, "--force")
	after, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("--force replaced the project directory")
	}
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("--force did not restore the project")
	}
	if !exists(t, filepath.Join(root, "notes.txt")) {
		t.Error("--force removed the kept files")
	}
}

func TestForcedArchiveRestoreKeepsTheFolder(t *testing.T) {
	h := newHarness(t)
	root := h.localProject("solo")
	h.mustRun("park", root)
	mk(t, root)
	writeFile(t, filepath.Join(root, "stale.txt"), "replace me\n", 0o644)

	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	h.mustRun("restore", root, "--force")
	after, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("--force replaced the project directory")
	}
	if exists(t, filepath.Join(root, "stale.txt")) {
		t.Error("--force should replace what was there")
	}
	if !exists(t, filepath.Join(root, "main.go")) {
		t.Error("the archived project was not restored")
	}
}

// --- dry run, push and sweep ------------------------------------------------

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("sorta")

	out := h.mustRun("park", root, "--dry-run")
	for _, want := range []string{"would delete", "would keep", "dry run"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output missing %q:\n%s", want, out)
		}
	}
	for _, untouched := range []string{"main.go", ".git", "node_modules", "notes.txt", ".env"} {
		if !exists(t, filepath.Join(root, untouched)) {
			t.Errorf("--dry-run deleted %s", untouched)
		}
	}
	if out := h.mustRun("list"); strings.Contains(out, "sorta") {
		t.Errorf("--dry-run recorded the project:\n%s", out)
	}
}

func TestDryRunStillRefusesWhatParkWouldRefuse(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("dirty")
	writeFile(t, filepath.Join(root, "main.go"), "package main // edited\n", 0o644)
	if _, err := h.run("park", root, "--dry-run"); err == nil {
		t.Error("a dry run should report the same blockers park does")
	}
}

func TestPushClearsTheBlockerAndParks(t *testing.T) {
	h := newHarness(t)
	root, remote := h.remoteProject("pushy")
	writeFile(t, filepath.Join(root, "extra.go"), "package main\n", 0o644)
	gitAt(t, root, "add", "extra.go")
	gitAt(t, root, "commit", "-qm", "local only")

	out := h.mustRun("park", root, "--push")
	if !strings.Contains(out, "pushed main") {
		t.Errorf("park --push should say what it pushed:\n%s", out)
	}
	if exists(t, filepath.Join(root, "extra.go")) {
		t.Error("the project was not parked after pushing")
	}
	// The commit really is on the remote now.
	if log := gitOut(t, remote, "log", "--oneline", "-1", "main"); !strings.Contains(log, "local only") {
		t.Errorf("the remote did not receive the commit: %s", log)
	}
	h.mustRun("restore", root)
	if !exists(t, filepath.Join(root, "extra.go")) {
		t.Error("the pushed commit did not come back")
	}
}

func TestPushCoversABranchWithNoUpstream(t *testing.T) {
	h := newHarness(t)
	root, remote := h.remoteProject("branchy")
	gitAt(t, root, "checkout", "-q", "-b", "side")
	writeFile(t, filepath.Join(root, "side.go"), "package main\n", 0o644)
	gitAt(t, root, "add", "side.go")
	gitAt(t, root, "commit", "-qm", "side work")

	h.mustRun("park", root, "--push")
	if branches := gitOut(t, remote, "branch", "--list"); !strings.Contains(branches, "side") {
		t.Errorf("the branch was not pushed: %s", branches)
	}
	h.mustRun("restore", root)
	if !exists(t, filepath.Join(root, "side.go")) {
		t.Error("the side branch did not come back")
	}
}

func TestPushDoesNotHelpWithUncommittedWork(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("dirty")
	writeFile(t, filepath.Join(root, "main.go"), "package main // edited\n", 0o644)
	_, err := h.run("park", root, "--push")
	if err == nil {
		t.Fatal("--push should not paper over uncommitted changes")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSweepListsAndParksWhatIsReady(t *testing.T) {
	h := newHarness(t)
	ready, _ := h.remoteProject("quiet-one")
	blocked, _ := h.remoteProject("has-work")
	writeFile(t, filepath.Join(blocked, "main.go"), "package main // edited\n", 0o644)
	local := h.localProject("no-remote")
	backdate(t, ready)
	backdate(t, blocked)
	backdate(t, local)

	h.stdin = "y\n"
	out := h.mustRun("sweep", h.base, "--older-than", "1d")
	for _, want := range []string{"NAME", "FREES", "LAST COMMIT", "STATUS",
		"quiet-one", "ready", "has-work", "uncommitted file", "no-remote", "no remote"} {
		if !strings.Contains(out, want) {
			t.Errorf("sweep output missing %q:\n%s", want, out)
		}
	}
	if !exists(t, filepath.Join(ready, "notes.txt")) || exists(t, filepath.Join(ready, ".git")) {
		t.Error("the ready project was not parked")
	}
	if !exists(t, filepath.Join(blocked, ".git")) {
		t.Error("a blocked project was parked anyway")
	}
	if !exists(t, filepath.Join(local, ".git")) {
		t.Error("a project with no remote was parked anyway")
	}
}

func TestSweepDryRunAndDeclineParkNothing(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("quiet-one")
	backdate(t, root)

	out := h.mustRun("sweep", h.base, "--older-than", "1d", "--dry-run")
	if strings.Contains(out, "[y/N]") {
		t.Errorf("--dry-run should not ask:\n%s", out)
	}
	if !exists(t, filepath.Join(root, ".git")) {
		t.Fatal("--dry-run parked a project")
	}

	h.stdin = "n\n"
	h.mustRun("sweep", h.base, "--older-than", "1d")
	if !exists(t, filepath.Join(root, ".git")) {
		t.Error("declining still parked the project")
	}
}

func TestSweepSkipsProjectsThatAreStillInUse(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("fresh") // committed just now
	h.stdin = "y\n"
	out := h.mustRun("sweep", h.base, "--older-than", "30d")
	if strings.Contains(out, "fresh") {
		t.Errorf("a project worked on today should not be swept:\n%s", out)
	}
	if !exists(t, filepath.Join(root, ".git")) {
		t.Error("a recent project was parked")
	}
}

func TestSweepSkipsAlreadyParkedProjects(t *testing.T) {
	h := newHarness(t)
	root, _ := h.remoteProject("parked-already")
	backdate(t, root)
	h.mustRun("park", root)

	out := h.mustRun("sweep", h.base, "--older-than", "1d", "--dry-run")
	if strings.Contains(out, "parked-already") {
		t.Errorf("an already parked project should not be listed:\n%s", out)
	}
}

func TestParseAge(t *testing.T) {
	cases := map[string]time.Duration{
		"30d": 30 * 24 * time.Hour,
		"6w":  6 * 7 * 24 * time.Hour,
		"3mo": 3 * 30 * 24 * time.Hour,
		"1y":  365 * 24 * time.Hour,
		"48h": 48 * time.Hour,
	}
	for in, want := range cases {
		got, err := parseAge(in)
		if err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "soon", "-5d", "d"} {
		if _, err := parseAge(bad); err == nil {
			t.Errorf("parseAge(%q) should fail", bad)
		}
	}
}

// backdate rewrites the repository's commit dates so a sweep sees it as idle,
// and puts the rewritten history on the remote so the project still counts as
// fully pushed.
func backdate(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	cmd := exec.Command("git", "commit", "--quiet", "--amend", "--no-edit", "--date", old)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+old)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("backdate: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "remote").Output(); err == nil && len(out) > 0 {
		gitAt(t, root, "push", "-q", "--force", "origin", "HEAD")
	}
}

func TestSweepPushMakesUnpushedProjectsReady(t *testing.T) {
	h := newHarness(t)
	root, remote := h.remoteProject("ahead")
	writeFile(t, filepath.Join(root, "extra.go"), "package main\n", 0o644)
	gitAt(t, root, "add", "extra.go")
	gitAt(t, root, "commit", "-qm", "local only")
	backdate(t, root)
	// backdate force-pushes, so put the repository back out in front — with an
	// old committer date, or the sweep will count it as worked on today.
	writeFile(t, filepath.Join(root, "later.go"), "package main\n", 0o644)
	gitAt(t, root, "add", "later.go")
	gitAtOld(t, root, "commit", "-qm", "still local")

	if out := h.mustRun("sweep", h.base, "--older-than", "1d", "--dry-run"); !strings.Contains(out, "unpushed commit") {
		t.Errorf("without --push the project should be blocked:\n%s", out)
	}

	h.stdin = "y\n"
	h.mustRun("sweep", h.base, "--older-than", "1d", "--push")
	if exists(t, filepath.Join(root, ".git")) {
		t.Error("sweep --push did not park the project")
	}
	if log := gitOut(t, remote, "log", "--oneline", "main"); !strings.Contains(log, "still local") {
		t.Errorf("the commit was not pushed before parking: %s", log)
	}
}
