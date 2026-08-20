// Package git answers the questions clav asks about a repository: where its
// root is, what it tracks, whether anything local is missing from its remote.
//
// Everything here shells out to the git binary. clav parks real working
// copies, so the answers must be the ones git itself would give — including
// the user's own config, credential helpers and ignore rules.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotInstalled is returned when the git binary cannot be found.
var ErrNotInstalled = errors.New("git is not installed")

// ErrNotRepo is returned for a directory that is not inside a git repository.
var ErrNotRepo = errors.New("not a git repository")

// Remote is a configured remote and its fetch URL.
type Remote struct {
	Name string
	URL  string
}

// Repo is the state of a working copy at the moment it was inspected.
type Repo struct {
	Root       string
	Remotes    []Remote
	Branch     string // empty when HEAD is detached
	Commit     string // empty for a repository with no commits
	Submodules []string
}

// Origin is the remote a parked project is restored from: "origin" if it
// exists, otherwise the first configured remote.
func (r *Repo) Origin() (Remote, bool) {
	for _, rem := range r.Remotes {
		if rem.Name == "origin" {
			return rem, true
		}
	}
	if len(r.Remotes) > 0 {
		return r.Remotes[0], true
	}
	return Remote{}, false
}

// AbsoluteURL turns a remote that is a relative local path into an absolute
// one, resolved against the repository it was configured in. clav stores the
// result and clones from it later, from a different working directory, where a
// relative path would mean somewhere else entirely.
func AbsoluteURL(root, url string) string {
	switch {
	case url == "":
		return url
	case strings.Contains(url, "://"):
		return url // scheme: https, ssh, git, file
	case filepath.IsAbs(url):
		return url
	}
	// host:path (scp syntax) has a colon before the first slash; a local
	// relative path does not.
	if i := strings.IndexByte(url, ':'); i >= 0 {
		if j := strings.IndexByte(url, '/'); j < 0 || i < j {
			return url
		}
	}
	if abs, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(url))); err == nil {
		return abs
	}
	return url
}

// Open inspects the repository whose root is dir. It fails when dir is not a
// repository, and when dir is inside one but is not its root — parking half a
// repository is never what the user meant.
func Open(ctx context.Context, dir string) (*Repo, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, ErrNotInstalled
	}
	top, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, ErrNotRepo
	}
	r := &Repo{Root: strings.TrimSpace(top)}

	if out, err := run(ctx, dir, "remote", "-v"); err == nil {
		seen := map[string]bool{}
		for _, line := range lines(out) {
			fields := strings.Fields(line)
			if len(fields) < 2 || seen[fields[0]] {
				continue
			}
			seen[fields[0]] = true
			r.Remotes = append(r.Remotes, Remote{Name: fields[0], URL: fields[1]})
		}
	}
	if out, err := run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		r.Branch = strings.TrimSpace(out)
	}
	if out, err := run(ctx, dir, "rev-parse", "HEAD"); err == nil {
		r.Commit = strings.TrimSpace(out)
	}
	if out, err := run(ctx, dir, "submodule", "--quiet", "foreach", "--recursive", "echo $sm_path"); err == nil {
		r.Submodules = lines(out)
	}
	return r, nil
}

// IsRepoRoot reports whether dir is the root of a repository.
func IsRepoRoot(ctx context.Context, dir string) bool {
	r, err := Open(ctx, dir)
	return err == nil && sameDir(r.Root, dir)
}

// TrackedFiles lists every path in the index, relative to the repository root.
func (r *Repo) TrackedFiles(ctx context.Context) ([]string, error) {
	out, err := run(ctx, r.Root, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("cannot list tracked files: %w", err)
	}
	return zeroSplit(out), nil
}

// IgnoredEntries lists ignored paths, relative to the repository root. A
// directory that is ignored in its entirety is reported once, with a trailing
// slash, instead of file by file.
func (r *Repo) IgnoredEntries(ctx context.Context) ([]string, error) {
	out, err := run(ctx, r.Root, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, fmt.Errorf("cannot list ignored files: %w", err)
	}
	return zeroSplit(out), nil
}

// DirtyTracked returns the porcelain status lines for tracked files. Untracked
// files are not included: park keeps them, so they are never a reason to stop.
func (r *Repo) DirtyTracked(ctx context.Context) ([]string, error) {
	out, err := run(ctx, r.Root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return nil, fmt.Errorf("cannot read git status: %w", err)
	}
	return lines(out), nil
}

// Stashes counts stash entries. They live only in .git, so parking a
// repository with stashes would throw them away.
func (r *Repo) Stashes(ctx context.Context) int {
	out, err := run(ctx, r.Root, "stash", "list")
	if err != nil {
		return 0
	}
	return len(lines(out))
}

// RemoteTimeout bounds the one network call clav makes. A remote that is down,
// or a host that black-holes the connection, must fail with an answer rather
// than hang forever. Override it with CLAV_REMOTE_TIMEOUT (a Go duration).
const RemoteTimeout = 20 * time.Second

// ErrRemoteTimeout is returned when the remote does not answer in time.
var ErrRemoteTimeout = errors.New("the remote did not answer in time")

func remoteTimeout() time.Duration {
	if v := os.Getenv("CLAV_REMOTE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return RemoteTimeout
}

// LsRemote asks the remote which commits it actually has. This is a network
// call on purpose: remote-tracking refs can be arbitrarily stale, and clav is
// about to delete the only other copy.
//
// It never waits indefinitely and never stops to ask for credentials: an
// unattended prompt is just another way to hang.
func (r *Repo) LsRemote(ctx context.Context, name string) ([]string, error) {
	timeout := remoteTimeout()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := run(tctx, r.Root, "ls-remote", "--heads", "--tags", name)
	if err != nil {
		// A deadline that fired here is ours, not the caller's cancellation.
		if ctx.Err() == nil && tctx.Err() != nil {
			return nil, fmt.Errorf("%w (%s)", ErrRemoteTimeout, timeout)
		}
		return nil, err
	}
	var shas []string
	for _, line := range lines(out) {
		if f := strings.Fields(line); len(f) >= 1 && len(f[0]) == 40 {
			shas = append(shas, f[0])
		}
	}
	return shas, nil
}

// Unpushed returns commits reachable from local refs but not from any of the
// given remote commits. Remote commits the repository does not have locally
// are skipped: they mean the remote is ahead, which is not clav's problem.
func (r *Repo) Unpushed(ctx context.Context, remoteShas []string) (count int, sample []string, err error) {
	have := make([]string, 0, len(remoteShas))
	for _, sha := range remoteShas {
		if _, err := run(ctx, r.Root, "cat-file", "-e", sha+"^{commit}"); err == nil {
			have = append(have, sha)
		}
	}
	if len(have) == 0 {
		return -1, nil, nil // nothing in common; the caller must decide
	}
	args := append([]string{"rev-list", "--count", "HEAD", "--branches", "--tags", "--not"}, have...)
	out, err := run(ctx, r.Root, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("cannot compare local commits with the remote: %w", err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &count); err != nil {
		return 0, nil, fmt.Errorf("cannot compare local commits with the remote: %w", err)
	}
	if count > 0 {
		args = append([]string{"log", "--oneline", "-n", "3", "HEAD", "--branches", "--tags", "--not"}, have...)
		if out, err := run(ctx, r.Root, args...); err == nil {
			sample = lines(out)
		}
	}
	return count, sample, nil
}

// UnpushedLocal counts commits that no remote-tracking ref knows about. It
// asks only what is already on disk, so it costs nothing and works offline —
// which is exactly what --force needs to be able to warn about.
func (r *Repo) UnpushedLocal(ctx context.Context) int {
	out, err := run(ctx, r.Root, "rev-list", "--count", "HEAD", "--branches", "--tags", "--not", "--remotes")
	if err != nil {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0
	}
	return n
}

// BranchesWithUnpushed lists local branches holding commits none of the given
// remote commits reach. These are exactly the branches a push has to cover.
func (r *Repo) BranchesWithUnpushed(ctx context.Context, remoteShas []string) ([]string, error) {
	have := make([]string, 0, len(remoteShas))
	for _, sha := range remoteShas {
		if _, err := run(ctx, r.Root, "cat-file", "-e", sha+"^{commit}"); err == nil {
			have = append(have, sha)
		}
	}
	out, err := run(ctx, r.Root, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, fmt.Errorf("cannot list branches: %w", err)
	}
	var behind []string
	for _, branch := range lines(out) {
		args := append([]string{"rev-list", "--count", branch, "--not"}, have...)
		count, cerr := run(ctx, r.Root, args...)
		if cerr != nil {
			return nil, fmt.Errorf("cannot compare %s with the remote: %w", branch, cerr)
		}
		var n int
		if _, serr := fmt.Sscanf(strings.TrimSpace(count), "%d", &n); serr == nil && n > 0 {
			behind = append(behind, branch)
		}
	}
	return behind, nil
}

// Push sends a branch to a remote and makes it the branch's upstream. It is
// the one thing clav does that changes anything outside the machine, so it is
// only ever called for a branch the remote is missing, and only when asked.
func (r *Repo) Push(ctx context.Context, remote, branch string) error {
	if _, err := run(ctx, r.Root, "push", "--quiet", "--set-upstream", remote, branch); err != nil {
		return fmt.Errorf("cannot push %s to %s: %w", branch, remote, err)
	}
	return nil
}

// LastCommit is when the newest commit on any branch was made. It is the
// closest thing git has to "when did anyone last work on this".
func LastCommit(ctx context.Context, dir string) (time.Time, error) {
	out, err := run(ctx, dir, "for-each-ref", "--sort=-committerdate", "--count=1",
		"--format=%(committerdate:unix)", "refs/heads")
	if err != nil {
		return time.Time{}, err
	}
	var unix int64
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &unix); err != nil {
		return time.Time{}, fmt.Errorf("no commits")
	}
	return time.Unix(unix, 0), nil
}

// CloneOptions configures Clone.
type CloneOptions struct {
	URL        string
	Dest       string
	RemoteName string
	Branch     string
	Submodules bool
}

// Clone fetches a repository into a directory that does not exist yet.
//
// Branch is a preference, not a requirement: a branch that only ever existed
// locally, or one deleted after a merge, must not stop the rest of the project
// from coming back. The caller checks where it landed.
func Clone(ctx context.Context, o CloneOptions) error {
	args := []string{"clone", "--quiet"}
	if o.RemoteName != "" && o.RemoteName != "origin" {
		args = append(args, "--origin", o.RemoteName)
	}
	if o.Submodules {
		args = append(args, "--recurse-submodules")
	}
	args = append(args, "--", o.URL, o.Dest)
	if _, err := run(ctx, "", args...); err != nil {
		return fmt.Errorf("cannot clone %s: %w", o.URL, err)
	}
	return nil
}

// CheckoutBranch moves a repository onto a branch, creating the local branch
// from the remote if necessary. It reports whether the branch exists.
func CheckoutBranch(ctx context.Context, dir, branch string) bool {
	_, err := run(ctx, dir, "checkout", "--quiet", branch)
	return err == nil
}

// HasCommit reports whether a repository contains a commit.
func HasCommit(ctx context.Context, dir, sha string) bool {
	if sha == "" {
		return false
	}
	_, err := run(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// CurrentBranch returns the branch a repository is on, or "" when detached.
func CurrentBranch(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Head returns the current commit of a repository.
func Head(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// Checkout moves a repository to a specific commit, leaving HEAD detached.
func Checkout(ctx context.Context, dir, commit string) error {
	if _, err := run(ctx, dir, "checkout", "--quiet", commit); err != nil {
		return fmt.Errorf("cannot check out %s: %w", short(commit), err)
	}
	return nil
}

// AddRemote restores a remote that the parked repository had configured.
func AddRemote(ctx context.Context, dir, name, url string) error {
	_, err := run(ctx, dir, "remote", "add", name, url)
	return err
}

// run executes git and returns its standard output. A failure carries git's
// own message, which is almost always the most useful thing clav can say.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Never block on a credential or host-key prompt: clav is not interactive.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.String(), ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return stdout.String(), err
		}
		return stdout.String(), errors.New(firstLine(msg))
	}
	return stdout.String(), nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func zeroSplit(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func sameDir(a, b string) bool { return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/") }
