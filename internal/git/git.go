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
//
// Branches whose work has already landed on the remote are left out — see
// UnmergedBranches. A year-old feature branch that was squash-merged still
// holds commits no remote has, and treating those as unfinished work would
// make park refuse every repository anyone actually works in.
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
	live, _, err := r.UnmergedBranches(ctx)
	if err != nil {
		return 0, nil, err
	}
	positives := append([]string{"HEAD"}, live...)
	positives = append(positives, "--tags")

	args := append(append([]string{"rev-list", "--count"}, positives...), "--not")
	out, err := run(ctx, r.Root, append(args, have...)...)
	if err != nil {
		return 0, nil, fmt.Errorf("cannot compare local commits with the remote: %w", err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &count); err != nil {
		return 0, nil, fmt.Errorf("cannot compare local commits with the remote: %w", err)
	}
	if count > 0 {
		args = append(append([]string{"log", "--oneline", "-n", "3"}, positives...), "--not")
		if out, err := run(ctx, r.Root, append(args, have...)...); err == nil {
			sample = lines(out)
		}
	}
	return count, sample, nil
}

// UnmergedBranches splits the local branches into those with work the remote
// has not seen and those whose work has already landed on it.
//
// Three things count as landed:
//
//	the branch is an ancestor of the remote's default branch (an ordinary merge)
//	the branch is an ancestor of the remote branch of the same name
//	the branch's changes, squashed into one commit, are already upstream
//
// The third is the case that matters in practice: a forge that squash-merges
// leaves every merged branch looking unpushed forever, because none of its
// commits exist on the remote by name.
func (r *Repo) UnmergedBranches(ctx context.Context) (unmerged, merged []string, err error) {
	out, err := run(ctx, r.Root, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, nil, fmt.Errorf("cannot list branches: %w", err)
	}
	upstream := r.defaultRemoteBranch(ctx)
	for _, branch := range lines(out) {
		if r.landed(ctx, branch, upstream) {
			merged = append(merged, branch)
			continue
		}
		unmerged = append(unmerged, branch)
	}
	return unmerged, merged, nil
}

// landed reports whether a branch's work is already on the remote.
func (r *Repo) landed(ctx context.Context, branch, upstream string) bool {
	for _, target := range []string{upstream, r.remoteTwin(ctx, branch)} {
		if target == "" {
			continue
		}
		if _, err := run(ctx, r.Root, "merge-base", "--is-ancestor", branch, target); err == nil {
			return true
		}
	}
	if upstream == "" {
		return false
	}
	return r.squashedInto(ctx, branch, upstream)
}

// squashedInto asks whether everything a branch changed is already upstream as
// a single commit. It builds the commit that a squash-merge of this branch
// would have produced and looks for its patch upstream. The synthesised commit
// is never referenced, so it is unreachable the moment this returns.
func (r *Repo) squashedInto(ctx context.Context, branch, upstream string) bool {
	base, err := run(ctx, r.Root, "merge-base", branch, upstream)
	if err != nil {
		return false
	}
	synth, err := run(ctx, r.Root, "commit-tree", branch+"^{tree}",
		"-p", strings.TrimSpace(base), "-m", "clav squash probe")
	if err != nil {
		return false
	}
	out, err := run(ctx, r.Root, "cherry", upstream, strings.TrimSpace(synth))
	if err != nil {
		return false
	}
	// "-" means git found this patch upstream already; "+" means it did not.
	for _, line := range lines(out) {
		return strings.HasPrefix(line, "-")
	}
	// No output at all: nothing to compare, so nothing is missing either.
	return true
}

// defaultRemoteBranch is the remote's main line of development.
func (r *Repo) defaultRemoteBranch(ctx context.Context) string {
	origin, ok := r.Origin()
	if !ok {
		return ""
	}
	if out, err := run(ctx, r.Root, "symbolic-ref", "--quiet", "--short",
		"refs/remotes/"+origin.Name+"/HEAD"); err == nil {
		return strings.TrimSpace(out)
	}
	for _, name := range []string{"main", "master", "trunk", "develop"} {
		ref := origin.Name + "/" + name
		if _, err := run(ctx, r.Root, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return ref
		}
	}
	return ""
}

// remoteTwin is the remote-tracking branch of the same name, if there is one.
func (r *Repo) remoteTwin(ctx context.Context, branch string) string {
	origin, ok := r.Origin()
	if !ok {
		return ""
	}
	ref := origin.Name + "/" + branch
	if _, err := run(ctx, r.Root, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return ""
	}
	return ref
}

// UnpushedLocal counts commits that no remote-tracking ref knows about, and
// that are not on a branch whose work has already landed. It asks only what is
// already on disk, so it costs nothing and works offline — which is what
// --force and sweep need.
func (r *Repo) UnpushedLocal(ctx context.Context) int {
	live, _, err := r.UnmergedBranches(ctx)
	if err != nil {
		return 0
	}
	args := append([]string{"rev-list", "--count", "HEAD"}, live...)
	args = append(args, "--tags", "--not", "--remotes")
	out, err := run(ctx, r.Root, args...)
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
// remote commits reach. These are exactly the branches a push has to cover —
// branches whose work already landed are left alone, so pushing never puts a
// long-merged branch back on the remote.
func (r *Repo) BranchesWithUnpushed(ctx context.Context, remoteShas []string) ([]string, error) {
	have := make([]string, 0, len(remoteShas))
	for _, sha := range remoteShas {
		if _, err := run(ctx, r.Root, "cat-file", "-e", sha+"^{commit}"); err == nil {
			have = append(have, sha)
		}
	}
	live, _, err := r.UnmergedBranches(ctx)
	if err != nil {
		return nil, err
	}
	var behind []string
	for _, branch := range live {
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

// Stash is one entry from the stash list.
type Stash struct {
	SHA     string
	Message string
}

// Stashes lists the stash entries, newest first — the order git shows them in.
func (r *Repo) StashList(ctx context.Context) ([]Stash, error) {
	out, err := run(ctx, r.Root, "stash", "list", "--format=%H%x1f%gs")
	if err != nil {
		return nil, fmt.Errorf("cannot list stashes: %w", err)
	}
	var out2 []Stash
	for _, line := range lines(out) {
		parts := strings.SplitN(line, "\x1f", 2)
		if len(parts) != 2 {
			continue
		}
		out2 = append(out2, Stash{SHA: strings.TrimSpace(parts[0]), Message: parts[1]})
	}
	return out2, nil
}

// StashCurrentChanges builds a stash commit from the uncommitted changes in the
// working copy without touching the working copy itself. It returns "" when
// there is nothing to save.
func (r *Repo) StashCurrentChanges(ctx context.Context) (string, error) {
	out, err := run(ctx, r.Root, "stash", "create")
	if err != nil {
		return "", fmt.Errorf("cannot capture the uncommitted changes: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// RescueRef is the ref namespace clav parks rescued work under. The refs only
// ever exist inside a bundle and inside a freshly restored clone.
const RescueRef = "refs/clav/rescue"

// Bundle writes the given commits to a bundle file, leaving out everything the
// remote already has so the file stays small. Each commit is written under
// RescueRef/<index>, in the order given.
func (r *Repo) Bundle(ctx context.Context, dest string, commits []string) error {
	if len(commits) == 0 {
		return errors.New("nothing to bundle")
	}
	refs := make([]string, 0, len(commits))
	for i, sha := range commits {
		ref := fmt.Sprintf("%s/%d", RescueRef, i)
		if _, err := run(ctx, r.Root, "update-ref", ref, sha); err != nil {
			return fmt.Errorf("cannot mark %s for rescue: %w", short(sha), err)
		}
		refs = append(refs, ref)
	}
	// The refs are only scaffolding for the bundle; the repository they live in
	// is about to be deleted anyway, but leave it as it was found.
	defer func() {
		for _, ref := range refs {
			_, _ = run(ctx, r.Root, "update-ref", "-d", ref)
		}
	}()

	args := append([]string{"bundle", "create", dest}, refs...)
	if _, err := run(ctx, r.Root, append(args, "--not", "--remotes")...); err == nil {
		return nil
	}
	// A repository with no remote-tracking refs cannot exclude anything.
	if _, err := run(ctx, r.Root, args...); err != nil {
		return fmt.Errorf("cannot save the rescued work: %w", err)
	}
	return nil
}

// UnbundleStashes pulls rescued commits out of a bundle and puts them back on
// the stash, oldest last, so `git stash list` reads as it did before parking.
func UnbundleStashes(ctx context.Context, dir, bundle string, messages []string) error {
	spec := RescueRef + "/*:" + RescueRef + "/*"
	if _, err := run(ctx, dir, "fetch", "--quiet", bundle, spec); err != nil {
		return fmt.Errorf("cannot read the rescued work from %s: %w", bundle, err)
	}
	// Stored newest last, because each store goes on top of the stash.
	for i := len(messages) - 1; i >= 0; i-- {
		ref := fmt.Sprintf("%s/%d", RescueRef, i)
		sha, err := run(ctx, dir, "rev-parse", ref)
		if err != nil {
			return fmt.Errorf("the rescued work is missing %s: %w", ref, err)
		}
		if _, err := run(ctx, dir, "stash", "store", "-m", messages[i], strings.TrimSpace(sha)); err != nil {
			return fmt.Errorf("cannot put the rescued work back on the stash: %w", err)
		}
		_, _ = run(ctx, dir, "update-ref", "-d", ref)
	}
	return nil
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

// HasHead reports whether a repository has a commit checked out. A fresh clone
// of a remote whose HEAD points at a branch that no longer exists has none:
// git leaves the working copy empty and says so in a warning nobody reads.
func HasHead(ctx context.Context, dir string) bool {
	_, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
}

// RemoteBranches lists the branches a clone got from its remote, in the order
// git reports them, without the remote prefix.
func RemoteBranches(ctx context.Context, dir, remote string) []string {
	out, err := run(ctx, dir, "for-each-ref", "--format=%(refname:short)", "refs/remotes/"+remote)
	if err != nil {
		return nil
	}
	prefix := remote + "/"
	var branches []string
	for _, ref := range lines(out) {
		name := strings.TrimPrefix(strings.TrimSpace(ref), prefix)
		if name == "" || name == "HEAD" {
			continue
		}
		branches = append(branches, name)
	}
	return branches
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
