package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (root, remote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "clav test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "clav test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	do(t, base, "init", "--bare", "-q", remote)

	root = filepath.Join(base, "project")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "main.go"), "package main\n")
	write(t, filepath.Join(root, "src", "app.go"), "package src\n")
	write(t, filepath.Join(root, ".gitignore"), "node_modules/\n.env\n")
	do(t, root, "init", "-q", "-b", "main")
	do(t, root, "add", ".")
	do(t, root, "commit", "-qm", "initial")
	do(t, root, "remote", "add", "origin", remote)
	do(t, root, "push", "-q", "-u", "origin", "main")

	write(t, filepath.Join(root, ".env"), "SECRET=1\n")
	write(t, filepath.Join(root, "notes.txt"), "notes\n")
	write(t, filepath.Join(root, "node_modules", "pad", "index.js"), "1\n")
	return root, remote
}

func do(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReportsTheRepositoryAsItIs(t *testing.T) {
	ctx := context.Background()
	root, remote := setup(t)

	repo, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Branch != "main" {
		t.Errorf("branch = %q, want main", repo.Branch)
	}
	if len(repo.Commit) != 40 {
		t.Errorf("commit = %q, want a full sha", repo.Commit)
	}
	origin, ok := repo.Origin()
	if !ok || origin.Name != "origin" || origin.URL != remote {
		t.Errorf("origin = %+v (%v), want %s", origin, ok, remote)
	}

	tracked, err := repo.TrackedFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{".gitignore": true, "main.go": true, "src/app.go": true}
	if len(tracked) != len(want) {
		t.Fatalf("tracked = %v, want %v", tracked, want)
	}
	for _, f := range tracked {
		if !want[f] {
			t.Errorf("unexpected tracked file %q", f)
		}
	}

	ignored, err := repo.IgnoredEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(ignored, " ")
	if !strings.Contains(joined, "node_modules/") || !strings.Contains(joined, ".env") {
		t.Errorf("ignored = %v, want node_modules/ and .env", ignored)
	}
	if strings.Contains(joined, "notes.txt") {
		t.Errorf("an untracked-but-not-ignored file must not be listed as ignored: %v", ignored)
	}
}

func TestOpenRefusesSomethingThatIsNotARepository(t *testing.T) {
	setup(t)
	if _, err := Open(context.Background(), t.TempDir()); err != ErrNotRepo {
		t.Errorf("err = %v, want ErrNotRepo", err)
	}
}

func TestDirtyTrackedIgnoresUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	root, _ := setup(t)
	repo, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}

	// notes.txt and .env exist but are not tracked: not a reason to stop.
	dirty, err := repo.DirtyTracked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Errorf("dirty = %v, want none", dirty)
	}

	write(t, filepath.Join(root, "main.go"), "package main // edited\n")
	dirty, err = repo.DirtyTracked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || !strings.Contains(dirty[0], "main.go") {
		t.Errorf("dirty = %v, want one entry for main.go", dirty)
	}
}

func TestStashesAreCounted(t *testing.T) {
	ctx := context.Background()
	root, _ := setup(t)
	repo, _ := Open(ctx, root)
	if n := repo.Stashes(ctx); n != 0 {
		t.Errorf("stashes = %d, want 0", n)
	}
	write(t, filepath.Join(root, "main.go"), "package main // wip\n")
	do(t, root, "stash", "push", "-q")
	if n := repo.Stashes(ctx); n != 1 {
		t.Errorf("stashes = %d, want 1", n)
	}
}

func TestUnpushedCountsWhatTheRemoteDoesNotHave(t *testing.T) {
	ctx := context.Background()
	root, _ := setup(t)
	repo, _ := Open(ctx, root)

	shas, err := repo.LsRemote(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	count, _, err := repo.Unpushed(ctx, shas)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 for a fully pushed repository", count)
	}

	write(t, filepath.Join(root, "extra.go"), "package main\n")
	do(t, root, "add", "extra.go")
	do(t, root, "commit", "-qm", "local only")
	count, sample, err := repo.Unpushed(ctx, shas)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if len(sample) != 1 || !strings.Contains(sample[0], "local only") {
		t.Errorf("sample = %v, want the unpushed subject", sample)
	}

	// Commits on a branch with no upstream count too, even once main itself is
	// back in step with the remote.
	do(t, root, "checkout", "-q", "-b", "side")
	write(t, filepath.Join(root, "side.go"), "package main\n")
	do(t, root, "add", "side.go")
	do(t, root, "commit", "-qm", "side work")
	do(t, root, "checkout", "-q", "main")
	do(t, root, "reset", "-q", "--hard", "origin/main")
	if count, _, _ := repo.Unpushed(ctx, shas); count != 2 {
		t.Errorf("count = %d, want the two commits only the side branch has", count)
	}
}

func TestUnpushedReportsNothingInCommon(t *testing.T) {
	ctx := context.Background()
	root, _ := setup(t)
	repo, _ := Open(ctx, root)
	count, _, err := repo.Unpushed(ctx, []string{strings.Repeat("0", 40)})
	if err != nil {
		t.Fatal(err)
	}
	if count != -1 {
		t.Errorf("count = %d, want -1 when no remote commit is known locally", count)
	}
}

func TestLsRemoteFailsWhenTheRemoteIsGone(t *testing.T) {
	ctx := context.Background()
	root, remote := setup(t)
	repo, _ := Open(ctx, root)
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LsRemote(ctx, "origin"); err == nil {
		t.Error("ls-remote should fail for a remote that no longer exists")
	}
}

func TestCloneBringsTheProjectBack(t *testing.T) {
	ctx := context.Background()
	root, remote := setup(t)
	repo, _ := Open(ctx, root)

	dest := filepath.Join(t.TempDir(), "clone")
	if err := Clone(ctx, CloneOptions{URL: remote, Dest: dest, RemoteName: "origin", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	head, err := Head(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	if head != repo.Commit {
		t.Errorf("clone is at %s, want %s", head, repo.Commit)
	}
	if _, err := os.Stat(filepath.Join(dest, "src", "app.go")); err != nil {
		t.Errorf("clone is missing tracked content: %v", err)
	}
	if err := Checkout(ctx, dest, repo.Commit); err != nil {
		t.Errorf("checkout: %v", err)
	}
}

func TestAbsoluteURLOnlyRewritesRelativeLocalPaths(t *testing.T) {
	root := "/home/me/Projects/app"
	cases := map[string]string{
		"https://github.com/me/app.git": "https://github.com/me/app.git",
		"ssh://git@host/me/app.git":     "ssh://git@host/me/app.git",
		"git@github.com:me/app.git":     "git@github.com:me/app.git",
		"/srv/git/app.git":              "/srv/git/app.git",
		"../remote.git":                 "/home/me/Projects/remote.git",
		"remotes/app.git":               "/home/me/Projects/app/remotes/app.git",
	}
	for in, want := range cases {
		if got := AbsoluteURL(root, in); got != want {
			t.Errorf("AbsoluteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLsRemoteGivesUpOnAHostThatNeverAnswers(t *testing.T) {
	ctx := context.Background()
	root, _ := setup(t)
	// 192.0.2.0/24 is reserved for documentation and routes nowhere, so the
	// connection hangs until something stops it. That something must be clav.
	do(t, root, "remote", "set-url", "origin", "git://192.0.2.1/blackhole.git")
	t.Setenv("CLAV_REMOTE_TIMEOUT", "1s")

	repo, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := repo.LsRemote(ctx, "origin"); !errors.Is(err, ErrRemoteTimeout) {
		t.Errorf("err = %v, want ErrRemoteTimeout", err)
	}
	if waited := time.Since(start); waited > 15*time.Second {
		t.Errorf("waited %s; the timeout did not apply", waited)
	}
}

func TestCancellationIsReportedAsCancellation(t *testing.T) {
	root, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	repo, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := repo.LsRemote(ctx, "origin"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
