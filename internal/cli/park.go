package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattkje/clav/internal/archive"
	"github.com/mattkje/clav/internal/git"
	"github.com/mattkje/clav/internal/project"
	"github.com/mattkje/clav/internal/state"
)

const parkUsage = `Usage: clav park [path] [--push] [--rescue] [--dry-run] [--force] [--keep-ignored] [--verbose]

Frees the disk a git project is using. Everything git tracks is deleted along
with .git and the ignored build and dependency directories; every other file
you have in the folder stays exactly where it is. 'clav restore' clones the
project back from its remote and puts your kept files back with it.

Parking is refused when anything would be lost: uncommitted changes, commits
the remote does not have, or a stash.

A repository with no remote has nowhere to be restored from, so clav archives
the whole directory instead.

  --push          push branches the remote is missing first, then park
  --rescue        save stashes and uncommitted changes into clav, then park
  --dry-run       show what would be deleted and kept, change nothing
  --force         park even though the remote is missing changes or unreachable
  --keep-ignored  keep ignored build and dependency directories too`

func (a *App) park(ctx context.Context, args []string) error {
	cmd := newCommand("park", parkUsage)
	force := cmd.fs.Bool("force", false, "park even with unpushed work")
	keepIgnored := cmd.fs.Bool("keep-ignored", false, "keep ignored directories")
	push := cmd.fs.Bool("push", false, "push what the remote is missing first")
	rescue := cmd.fs.Bool("rescue", false, "save stashes and uncommitted changes")
	dryRun := cmd.fs.Bool("dry-run", false, "show what would happen")
	if err := cmd.parse(args); err != nil {
		return err
	}
	arg, note, err := a.target(cmd)
	if err != nil {
		return err
	}
	a.ui.Verbose = *cmd.verbose
	opts := parkOptions{
		force:       *force,
		keepIgnored: *keepIgnored,
		push:        *push,
		rescue:      *rescue,
		dryRun:      *dryRun,
	}
	// Resolve the working directory before anything is deleted; os.Getwd stops
	// working the moment the directory it names is gone. It is canonicalised so
	// it can be compared with the project path, which always is.
	startDir, _ := a.workingDir()
	if canon, cerr := project.Canonical(startDir); cerr == nil {
		startDir = canon
	}

	return a.parkPath(ctx, arg, note, opts, startDir)
}

// parkPath parks one project. It is what `clav park` does, and what each
// project a sweep picks goes through — a swept project gets exactly the same
// checks as one parked by hand.
func (a *App) parkPath(ctx context.Context, arg, note string, opts parkOptions, startDir string) error {
	ref, err := project.Resolve(arg, a.Store.Root())
	if err != nil {
		return err
	}

	current, err := a.Store.Load()
	if err != nil {
		return err
	}
	if existing, ok := current.Find(ref.Key); ok {
		return fmt.Errorf("%s is already parked (%s)\n"+
			"       run 'clav restore %s' to bring it back",
			ref.Display(), RelTime(existing.CreatedAt, time.Now()), ref.Display())
	}

	repo, err := git.Open(ctx, ref.Path)
	switch {
	case errors.Is(err, git.ErrNotInstalled):
		return errors.New("clav needs git, and git is not installed")
	case errors.Is(err, git.ErrNotRepo):
		return fmt.Errorf("%s is not a git repository\n"+
			"       clav only parks git projects, so there is always a way back", ref.Display())
	case err != nil:
		return err
	}
	if root, cerr := project.Canonical(repo.Root); cerr == nil && root != ref.Path {
		return fmt.Errorf("%s is inside a git repository, not the root of one\n"+
			"       park the repository instead: clav park %s", ref.Display(), project.Shorten(root))
	}
	if note != "" {
		a.ui.Detail("%s (%s)", ref.Display(), note)
	}

	origin, hasRemote := repo.Origin()
	if !hasRemote {
		return a.parkArchive(ctx, ref, opts, startDir)
	}
	return a.parkRemote(ctx, ref, repo, origin, opts, startDir)
}

// parkOptions are the choices a park is run with.
type parkOptions struct {
	force       bool
	keepIgnored bool
	push        bool
	rescue      bool
	dryRun      bool
}

// parkRemote deletes the content the remote already has, leaving behind every
// file that only exists locally.
func (a *App) parkRemote(ctx context.Context, ref project.Ref, repo *git.Repo, origin git.Remote,
	opts parkOptions, startDir string) error {

	if repo.Commit == "" {
		return fmt.Errorf("%s has no commits yet; there is nothing on %s to restore from",
			ref.Display(), origin.Name)
	}
	if opts.force {
		a.warnWhatForceOverrides(ctx, repo)
	} else if err := a.checkSafeToPark(ctx, ref, repo, origin, opts); err != nil {
		return err
	}

	step := a.ui.Step("Listing tracked files")
	doomed, err := a.doomedPaths(ctx, repo, opts.keepIgnored)
	if err != nil {
		step.Fail()
		return err
	}
	step.Done()
	a.ui.Detail("%s", strings.Join(namesOf(doomed, 8), ", "))

	if opts.dryRun {
		if opts.rescue {
			if n := a.rescuableCount(ctx, repo); n > 0 {
				a.ui.Line("would rescue  %-18s → saved into %s",
					plural2(n, "stash entry"), project.Shorten(a.Store.RescueDir()))
			}
		}
		return a.reportDryRun(ref, doomed)
	}

	// Record the project before deleting anything, so an interrupted park
	// leaves a restorable entry rather than a half-emptied folder.
	id, cycle, err := a.nextID(ref.Key)
	if err != nil {
		return err
	}

	// Save whatever has nowhere else to live, before .git — the only thing that
	// holds it — is deleted.
	rescueRel, rescueMessages := "", []string(nil)
	if opts.rescue {
		rescueRel, rescueMessages, err = a.rescueWork(ctx, repo, id)
		if err != nil {
			return err
		}
	}
	keepRescue := false
	defer func() {
		if rescueRel != "" && !keepRescue {
			_ = os.Remove(a.Store.Resolve(rescueRel))
		}
	}()

	record := state.Project{
		ID:             id,
		Kind:           state.KindRemote,
		Name:           ref.Name,
		OriginalPath:   ref.Path,
		PathKey:        ref.Key,
		CreatedAt:      time.Now().UTC().Truncate(time.Second),
		Cycle:          cycle,
		ClavVersion:    Version,
		RemoteName:     origin.Name,
		RemoteURL:      git.AbsoluteURL(ref.Path, origin.URL),
		Branch:         repo.Branch,
		Commit:         repo.Commit,
		Submodules:     len(repo.Submodules) > 0,
		Rescue:         rescueRel,
		RescueMessages: rescueMessages,
	}
	if err := a.Store.Update(func(f *state.File) error {
		if _, ok := f.Find(ref.Key); ok {
			return fmt.Errorf("%s was parked by another process; nothing was deleted", ref.Display())
		}
		f.Upsert(record)
		return nil
	}); err != nil {
		return err
	}
	keepRescue = true

	step = a.ui.Step("Deleting tracked content")
	purged, err := project.Remove(ref.Path, doomed)
	if err != nil {
		step.Fail()
		return fmt.Errorf("%s is parked, but part of it could not be deleted: %w\n"+
			"       delete the rest by hand, or run 'clav remove %s' to forget the entry",
			ref.Display(), err, ref.Display())
	}
	step.Done()
	a.ui.Detail("%d files, %d directories, %s", purged.Files, purged.Dirs, HumanSize(purged.Bytes))

	kept, keptBytes, err := project.Entries(ref.Path)
	if err != nil {
		return err
	}
	folderGone := false
	if kept == 0 && project.IsEmpty(ref.Path) {
		if os.Remove(ref.Path) == nil {
			folderGone = true
		}
	}

	if err := a.Store.Update(func(f *state.File) error {
		p, ok := f.Find(ref.Key)
		if !ok {
			return nil
		}
		p.FreedBytes = purged.Bytes
		p.KeptFiles = kept
		p.OriginalSize = purged.Bytes + keptBytes
		f.Upsert(p)
		return nil
	}); err != nil {
		a.ui.Warn("could not record the final size: %v", err)
	}

	summary := fmt.Sprintf("%s parked · %s freed", a.ui.bold(ref.Name), HumanSize(purged.Bytes))
	switch {
	case kept > 0:
		summary += fmt.Sprintf(" · %s kept", plural2(kept, "file"))
	case folderGone:
		summary += " · folder removed"
	}
	if n := len(rescueMessages); n > 0 {
		summary += fmt.Sprintf(" · %s rescued", plural2(n, "stash entry"))
	}
	a.ui.Result("%s", summary)
	a.shellHint(startDir, ref, folderGone)
	return nil
}

// warnWhatForceOverrides says out loud what --force is about to destroy. The
// checks are local only, so they cost nothing even when the remote is the
// reason --force was reached for.
func (a *App) warnWhatForceOverrides(ctx context.Context, repo *git.Repo) {
	if dirty, err := repo.DirtyTracked(ctx); err == nil && len(dirty) > 0 {
		a.ui.Warn("--force: discarding uncommitted changes to %s", plural2(len(dirty), "tracked file"))
	}
	if n := repo.Stashes(ctx); n > 0 {
		a.ui.Warn("--force: discarding %s; a stash cannot be recovered", plural2(n, "stash entry"))
	}
	if n := repo.UnpushedLocal(ctx); n > 0 {
		a.ui.Warn("--force: discarding %s no remote-tracking branch has; "+
			"they cannot be recovered", plural2(n, "commit"))
	}
}

// rescueWork saves the stash entries and the uncommitted changes into a git
// bundle in clav's own storage. Nothing goes to the remote: this is work the
// user never chose to publish, and clav does not choose for them.
//
// The bundle carries only what the remote does not already have, so it is
// normally a few kilobytes.
func (a *App) rescueWork(ctx context.Context, repo *git.Repo, id string) (string, []string, error) {
	stashes, err := repo.StashList(ctx)
	if err != nil {
		return "", nil, err
	}
	commits := make([]string, 0, len(stashes)+1)
	messages := make([]string, 0, len(stashes)+1)
	for _, st := range stashes {
		commits = append(commits, st.SHA)
		messages = append(messages, st.Message)
	}

	// The uncommitted changes become the newest stash entry, which is where
	// someone looking for them will expect to find them.
	wip, err := repo.StashCurrentChanges(ctx)
	if err != nil {
		return "", nil, err
	}
	if wip != "" {
		commits = append([]string{wip}, commits...)
		messages = append([]string{"clav rescue: uncommitted changes when parked"}, messages...)
	}
	if len(commits) == 0 {
		return "", nil, nil
	}

	rel := filepath.Join("rescue", id+".bundle")
	step := a.ui.Step("Rescuing local work")
	if err := repo.Bundle(ctx, a.Store.Resolve(rel), commits); err != nil {
		step.Fail()
		return "", nil, err
	}
	step.Done()
	for _, m := range messages {
		a.ui.Detail("rescued %s", m)
	}
	return filepath.ToSlash(rel), messages, nil
}

// rescuableCount is how many stash entries a rescue would save, counting the
// uncommitted changes as one.
func (a *App) rescuableCount(ctx context.Context, repo *git.Repo) int {
	n := repo.Stashes(ctx)
	if dirty, err := repo.DirtyTracked(ctx); err == nil && len(dirty) > 0 {
		n++
	}
	return n
}

// doomedPaths is everything git can hand back: the index, .git itself, the
// submodule working copies, and the ignored directories a build recreates.
// Nothing else is ever in this list.
func (a *App) doomedPaths(ctx context.Context, repo *git.Repo, keepIgnored bool) ([]string, error) {
	doomed, err := repo.TrackedFiles(ctx)
	if err != nil {
		return nil, err
	}
	doomed = append(doomed, ".git")
	doomed = append(doomed, repo.Submodules...)
	if keepIgnored {
		return doomed, nil
	}
	ignored, err := repo.IgnoredEntries(ctx)
	if err != nil {
		return nil, err
	}
	for _, rel := range ignored {
		if project.IsJunk(rel) {
			doomed = append(doomed, rel)
		}
	}
	return doomed, nil
}

// reportDryRun says what a park would do and changes nothing.
func (a *App) reportDryRun(ref project.Ref, doomed []string) error {
	going, err := project.Measure(ref.Path, doomed)
	if err != nil {
		return err
	}
	total, totalBytes, err := project.Entries(ref.Path)
	if err != nil {
		return err
	}
	kept, keptBytes := total-going.Files, totalBytes-going.Bytes
	if kept < 0 {
		kept = 0
	}
	if keptBytes < 0 {
		keptBytes = 0
	}

	a.ui.Line("would delete  %-18s → %s freed", plural2(going.Files, "file"), HumanSize(going.Bytes))
	a.ui.Line("would keep    %-18s → %s", plural2(kept, "file"), HumanSize(keptBytes))
	a.ui.Detail("%s", strings.Join(keptNames(ref.Path, doomed, 6), ", "))
	a.ui.Result("%s dry run · nothing changed", a.ui.bold(ref.Name))
	return nil
}

// namesOf summarises a delete list by its top-level entries.
func namesOf(rels []string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, rel := range rels {
		top := strings.TrimSuffix(strings.SplitN(filepath.ToSlash(rel), "/", 2)[0], "/")
		if top == "" || seen[top] {
			continue
		}
		seen[top] = true
		out = append(out, top)
	}
	return elide(out, max)
}

// keptNames lists the top-level entries a park would leave behind.
func keptNames(root string, doomed []string, max int) []string {
	going := map[string]bool{}
	for _, name := range namesOf(doomed, len(doomed)+1) {
		going[name] = true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !going[e.Name()] {
			out = append(out, e.Name())
		}
	}
	return elide(out, max)
}

func elide(names []string, max int) []string {
	if len(names) <= max {
		return names
	}
	return append(append([]string{}, names[:max]...),
		fmt.Sprintf("and %d more", len(names)-max))
}

// checkSafeToPark refuses to delete anything the remote cannot give back.
// With push, the one blocker clav can clear on its own — commits the remote
// has not seen — is cleared instead of reported.
func (a *App) checkSafeToPark(ctx context.Context, ref project.Ref, repo *git.Repo, origin git.Remote, opts parkOptions) error {
	step := a.ui.Step("Checking for local work")
	dirty, err := repo.DirtyTracked(ctx)
	if err != nil {
		step.Fail()
		return err
	}
	// With --rescue these are not blockers: the work is about to be saved into
	// clav's own storage, so deleting .git no longer loses it.
	if len(dirty) > 0 && !opts.rescue {
		step.Fail()
		return fmt.Errorf("%s has uncommitted changes to %s\n%s\n"+
			"       commit and push them, save them with 'clav park --rescue',\n"+
			"       or park anyway with --force",
			ref.Display(), plural2(len(dirty), "tracked file"), indent(dirty, 3))
	}
	if n := repo.Stashes(ctx); n > 0 && !opts.rescue {
		step.Fail()
		return fmt.Errorf("%s has %s; a stash lives only in .git and would be lost\n"+
			"       save it with 'clav park --rescue', apply or drop it,\n"+
			"       or park anyway with --force", ref.Display(), plural2(n, "stash entry"))
	}
	step.Done()

	step = a.ui.Step("Asking the remote")
	shas, err := repo.LsRemote(ctx, origin.Name)
	if err != nil {
		step.Fail()
		return fmt.Errorf("cannot reach %s (%s): %w\n"+
			"       clav will not delete code it cannot see a copy of; park anyway with --force\n"+
			"       (set CLAV_REMOTE_TIMEOUT to wait longer than %s)",
			origin.Name, origin.URL, err, git.RemoteTimeout)
	}
	count, sample, err := repo.Unpushed(ctx, shas)
	if err != nil {
		step.Fail()
		return err
	}
	step.Done()
	if _, landed, lerr := repo.UnmergedBranches(ctx); lerr == nil && len(landed) > 0 {
		a.ui.Detail("ignoring %s already merged into %s: %s",
			plural2(len(landed), "branch"), origin.Name, strings.Join(elide(landed, 5), ", "))
	}
	switch {
	case count < 0:
		return fmt.Errorf("%s has no commit in common with %s\n"+
			"       run 'git fetch %s' first, or park anyway with --force",
			ref.Display(), origin.Name, origin.Name)
	case count > 0 && opts.push:
		if err := a.pushMissing(ctx, repo, origin, shas); err != nil {
			return err
		}
		// Ask the remote again rather than assuming the push covered
		// everything: tags, for one, are not branches.
		shas, err = repo.LsRemote(ctx, origin.Name)
		if err != nil {
			return fmt.Errorf("cannot reach %s after pushing: %w", origin.Name, err)
		}
		count, sample, err = repo.Unpushed(ctx, shas)
		if err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%s still has %s that %s does not have after pushing\n%s\n"+
				"       push them by hand, or park anyway with --force",
				ref.Display(), plural2(count, "commit"), origin.Name, indent(sample, 3))
		}
	case count > 0:
		return fmt.Errorf("%s has %s that %s does not have\n%s\n"+
			"       push them with 'clav park --push', or park anyway with --force",
			ref.Display(), plural2(count, "commit"), origin.Name, indent(sample, 3))
	}
	return nil
}

// pushMissing sends every branch the remote is behind on. Each branch is named
// as it goes: this is the one thing clav does that other people can see.
func (a *App) pushMissing(ctx context.Context, repo *git.Repo, origin git.Remote, shas []string) error {
	branches, err := repo.BranchesWithUnpushed(ctx, shas)
	if err != nil {
		return err
	}
	if len(branches) == 0 {
		return nil
	}
	for _, branch := range branches {
		step := a.ui.Step("Pushing " + branch)
		if err := repo.Push(ctx, origin.Name, branch); err != nil {
			step.Fail()
			return err
		}
		step.Done()
		a.ui.Note("  pushed %s to %s", branch, origin.Name)
	}
	return nil
}

// parkArchive is the fallback for a repository with no remote: there is
// nowhere to clone it back from, so the whole directory is archived.
func (a *App) parkArchive(ctx context.Context, ref project.Ref, opts parkOptions, startDir string) error {
	step := a.ui.Step("Scanning project")
	scan, err := project.Scan(ref.Path, nil)
	if err != nil {
		step.Fail()
		return err
	}
	step.Done()
	a.ui.Detail("%d files, %d directories, %d symlinks", scan.Files, scan.Dirs, scan.Symlinks)
	for _, skipped := range scan.Skipped {
		a.ui.Detail("skipping socket %s", skipped)
	}

	if opts.dryRun {
		a.ui.Line("would archive %s (%s) and delete the folder",
			plural2(scan.Files, "file"), HumanSize(scan.Size))
		a.ui.Result("%s dry run · nothing changed (no remote, so the whole directory is archived)",
			a.ui.bold(ref.Name))
		return nil
	}

	id, cycle, err := a.nextID(ref.Key)
	if err != nil {
		return err
	}
	rel := filepath.Join("archives", id+archive.Extension)
	dest := a.Store.Resolve(rel)

	step = a.ui.Step("Creating archive")
	manifest, err := archive.Create(ctx, archive.CreateOptions{
		Root:    ref.Path,
		Dest:    dest,
		TempDir: a.Store.TempDir(),
	})
	if err != nil {
		step.Fail()
		return err
	}
	step.Done()
	a.ui.Detail("%s (%d entries, %s uncompressed)", rel, manifest.Entries, HumanSize(manifest.TotalBytes))
	for _, w := range manifest.Warnings {
		a.ui.Warn("%s", w)
	}

	// From here on, a failure must not leave a stray archive behind.
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(dest)
		}
	}()

	// Verify: read every byte back, confirm structure and checksum.
	step = a.ui.Step("Verifying archive")
	verified, err := archive.Verify(ctx, archive.VerifyOptions{
		Path:       dest,
		SHA256:     manifest.SHA256,
		Entries:    manifest.Entries,
		TreeSHA256: manifest.TreeSHA256,
	})
	if err != nil {
		step.Fail()
		return err
	}
	step.Done()
	a.ui.Detail("sha256 %s", manifest.SHA256)
	if verified.Entries != scan.Entries {
		a.ui.Warn("the project changed while it was being archived (%d entries scanned, %d archived); "+
			"the archive is complete and verified", scan.Entries, verified.Entries)
	}

	record := state.Project{
		ID:           id,
		Kind:         state.KindArchive,
		Name:         ref.Name,
		OriginalPath: ref.Path,
		PathKey:      ref.Key,
		Archive:      filepath.ToSlash(rel),
		Compression:  manifest.Compression,
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
		OriginalSize: scan.Size,
		ArchiveSize:  manifest.Size,
		FreedBytes:   scan.Size - manifest.Size,
		SHA256:       manifest.SHA256,
		EntryCount:   manifest.Entries,
		TreeSHA256:   manifest.TreeSHA256,
		Cycle:        cycle,
		ClavVersion:  Version,
	}
	if err := a.Store.Update(func(f *state.File) error {
		if _, ok := f.Find(ref.Key); ok {
			return fmt.Errorf("%s was parked by another process; nothing was deleted", ref.Display())
		}
		f.Upsert(record)
		return nil
	}); err != nil {
		return err
	}
	keep = true
	a.ui.Detail("recorded as %s", id)

	// Only now remove the original. Cancellation is intentionally ignored
	// here: the archive is already safe on disk, and a half-deleted directory
	// is worse than a fully deleted one.
	step = a.ui.Step("Removing original")
	if err := os.RemoveAll(ref.Path); err != nil {
		step.Fail()
		return fmt.Errorf("the project is parked and verified, but %s could not be removed: %w\n"+
			"       remove it by hand, or run 'clav restore --force %s' to undo",
			ref.Display(), err, ref.Display())
	}
	step.Done()

	a.ui.Result("%s parked · %s → %s · no remote, archived",
		a.ui.bold(ref.Name), HumanSize(scan.Size), HumanSize(manifest.Size))
	a.shellHint(startDir, ref, true)
	return nil
}

// shellHint mentions the obvious next move when the shell is standing in a
// directory clav has just emptied or removed.
func (a *App) shellHint(startDir string, ref project.Ref, folderGone bool) {
	if startDir == "" || !isInside(startDir, ref.Path) {
		return
	}
	if folderGone {
		a.ui.Note("  your shell is in the deleted directory; run: cd %s",
			project.Shorten(filepath.Dir(ref.Path)))
	}
}

// indent formats a few lines of git output as part of an error message.
func indent(lines []string, max int) string {
	if len(lines) > max {
		lines = append(append([]string{}, lines[:max]...),
			fmt.Sprintf("... and %d more", len(lines)-max))
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, "         "+strings.TrimSpace(l))
	}
	return strings.Join(out, "\n")
}

// isInside reports whether path is root or sits under it.
func isInside(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path,
		strings.TrimRight(root, string(os.PathSeparator))+string(os.PathSeparator))
}

// nextID derives an archive ID for a project: the stable path key plus a cycle
// counter, so repeated park/restore cycles of the same directory never collide
// with a leftover archive.
func (a *App) nextID(pathKey string) (string, int, error) {
	var id string
	var cycle int
	err := a.Store.Update(func(f *state.File) error {
		if f.Counters == nil {
			f.Counters = map[string]int{}
		}
		for {
			f.Counters[pathKey]++
			cycle = f.Counters[pathKey]
			id = fmt.Sprintf("%s-%03d", pathKey[:12], cycle)
			candidate := filepath.Join(a.Store.ArchivesDir(), id+archive.Extension)
			if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			// An archive with that name already exists (an orphan from an
			// interrupted run); take the next number.
		}
	})
	if err != nil {
		return "", 0, err
	}
	return id, cycle, nil
}
