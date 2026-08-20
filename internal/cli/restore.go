package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mattkje/clav/internal/archive"
	"github.com/mattkje/clav/internal/git"
	"github.com/mattkje/clav/internal/project"
	"github.com/mattkje/clav/internal/state"
)

const restoreUsage = `Usage: clav restore [path] [--force] [--keep] [--verbose]

Puts a parked project back at its original location: clones it from its remote
again and moves the files you kept back in beside it.

With no path, clav restores the parked project you are standing in — parking
leaves the folder behind whenever there was anything to keep. Otherwise <path>
is the location the project had when it was parked.

  --force   replace whatever is at that location now
  --keep    leave the project listed as parked instead of releasing it`

func (a *App) restore(ctx context.Context, args []string) error {
	cmd := newCommand("restore", restoreUsage)
	force := cmd.fs.Bool("force", false, "replace an existing directory")
	keep := cmd.fs.Bool("keep", false, "keep the entry after restoring")
	if err := cmd.parse(args); err != nil {
		return err
	}
	a.ui.Verbose = *cmd.verbose

	current, err := a.Store.Load()
	if err != nil {
		return err
	}

	ref, record, err := a.restoreTarget(cmd, current)
	if err != nil {
		return err
	}

	parent := filepath.Dir(ref.Path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", project.Shorten(parent), err)
	}

	if record.KindOf() == state.KindRemote {
		return a.restoreRemote(ctx, ref, record, *force, *keep)
	}
	return a.restoreArchive(ctx, ref, record, *force, *keep)
}

// restoreTarget picks the project to restore. With no argument it is the
// project you are standing in: parking leaves the folder behind whenever there
// were files to keep, so `cd project && clav restore` is the obvious way to ask
// for it back.
func (a *App) restoreTarget(cmd *command, current *state.File) (project.Ref, state.Project, error) {
	if cmd.fs.NArg() > 0 {
		arg, err := cmd.one("project path")
		if err != nil {
			return project.Ref{}, state.Project{}, err
		}
		ref, err := project.Locate(arg)
		if err != nil {
			return project.Ref{}, state.Project{}, err
		}
		record, ok := current.Find(ref.Key)
		if !ok {
			return project.Ref{}, state.Project{}, fmt.Errorf(
				"no parked project at %s\n       run 'clav list' to see what is parked", ref.Display())
		}
		return ref, record, nil
	}

	cwd, err := a.workingDir()
	if err != nil {
		return project.Ref{}, state.Project{}, err
	}
	// Walk up: the shell may be in a subdirectory of the parked project, one
	// that only survived because it held files clav kept.
	dir, err := project.Canonical(cwd)
	if err != nil {
		return project.Ref{}, state.Project{}, err
	}
	home, _ := project.Canonical(mustHome())
	for {
		ref, lerr := project.Locate(dir)
		if lerr == nil {
			if record, ok := current.Find(ref.Key); ok {
				a.ui.Detail("%s (current directory)", ref.Display())
				return ref, record, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir || dir == home {
			break
		}
		dir = parent
	}
	return project.Ref{}, state.Project{}, usagef(
		"nothing parked at %s\n       name the project instead: clav restore <path>\n"+
			"       run 'clav list' to see what is parked", project.Shorten(cwd))
}

func mustHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return string(os.PathSeparator)
	}
	return home
}

// restoreRemote clones the project back and merges the kept files into it.
func (a *App) restoreRemote(ctx context.Context, ref project.Ref, record state.Project, force, keep bool) error {
	if record.RemoteURL == "" {
		return fmt.Errorf("%s has no remote recorded; clav cannot rebuild it", ref.Display())
	}
	parent := filepath.Dir(ref.Path)

	// The folder is expected to still be there holding the kept files. What is
	// not expected is a repository: that is someone else's work, not ours.
	existing := false
	if _, err := os.Lstat(ref.Path); err == nil {
		existing = true
		if _, gerr := os.Lstat(filepath.Join(ref.Path, ".git")); gerr == nil && !force {
			return fmt.Errorf("a git repository already exists at %s\n\n       Use --force to replace it.", ref.Display())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	staging := filepath.Join(parent, fmt.Sprintf(".clav-restore-%s", record.ID))
	_ = os.RemoveAll(staging)
	settled := false
	defer func() {
		if !settled {
			_ = os.RemoveAll(staging)
		}
	}()

	step := a.ui.Step("Cloning")
	if err := git.Clone(ctx, git.CloneOptions{
		URL:        record.RemoteURL,
		Dest:       staging,
		RemoteName: record.RemoteName,
		Branch:     record.Branch,
		Submodules: record.Submodules,
	}); err != nil {
		step.Fail()
		return err
	}
	step.Done()

	if err := a.settle(ctx, staging, record); err != nil {
		return err
	}

	// Put the rescued stash entries back before the clone moves into place, so
	// a failure here leaves the parked entry — and the bundle — untouched.
	if record.Rescue != "" {
		step = a.ui.Step("Restoring rescued work")
		bundle := a.Store.Resolve(record.Rescue)
		if err := git.UnbundleStashes(ctx, staging, bundle, record.RescueMessages); err != nil {
			step.Fail()
			return err
		}
		step.Done()
	}

	// Merge the clone into the folder that is already there, rather than
	// swapping directories: the user's shell is very often standing in it, and
	// a rename would leave that shell inside a directory clav then deletes.
	moved := 0
	if existing {
		if force {
			// --force means "the repository in the way is not the one I want".
			// Only that goes; the files clav kept are merged as usual.
			if err := os.RemoveAll(filepath.Join(ref.Path, ".git")); err != nil {
				return err
			}
		}
		step = a.ui.Step("Merging kept files")
		res, err := project.Overlay(staging, ref.Path)
		if err != nil {
			step.Fail()
			return fmt.Errorf("cannot put %s back together: %w\n"+
				"       the rest of the clone is at %s", ref.Display(), err, project.Shorten(staging))
		}
		step.Done()
		moved = res.Moved
		for _, c := range res.Conflicts {
			a.ui.Warn("%s exists in the repository too; your copy is at %s.clav-kept", c, c)
		}
		if err := os.RemoveAll(staging); err != nil {
			a.ui.Warn("could not delete %s: %v", project.Shorten(staging), err)
		}
		settled = true
	} else {
		if err := os.Rename(staging, ref.Path); err != nil {
			return fmt.Errorf("cannot place the restored project at %s: %w", ref.Display(), err)
		}
		settled = true
	}

	if err := a.release(ref, record, keep); err != nil {
		return err
	}

	summary := fmt.Sprintf("%s restored · %s", a.ui.bold(record.Name), ref.Display())
	if record.KeptFiles > 0 {
		summary += fmt.Sprintf(" · %s kept in place", plural2(record.KeptFiles, "file"))
	}
	if n := len(record.RescueMessages); n > 0 {
		summary += fmt.Sprintf(" · %s back on the stash", plural2(n, "entry"))
		// The stash commits carry their index state, so a pop with --index puts
		// back exactly what was staged.
		a.ui.Note("  see them with 'git stash list'; 'git stash pop --index' keeps what was staged")
	}
	a.ui.Detail("%d entries merged in", moved)
	a.ui.Result("%s", summary)
	return nil
}

// settle puts the clone on the branch and commit the project was parked at, as
// far as the remote still has them, and says plainly when it cannot. A branch
// that was never pushed, or has been deleted since, must not cost the user the
// rest of the project.
func (a *App) settle(ctx context.Context, dir string, record state.Project) error {
	remote := record.RemoteName
	if remote == "" {
		remote = "origin"
	}

	onBranch := record.Branch != "" && git.CheckoutBranch(ctx, dir, record.Branch)
	if record.Branch != "" && !onBranch {
		fallback := git.CurrentBranch(ctx, dir)
		if fallback == "" {
			fallback = "the default branch"
		}
		a.ui.Warn("%s is not on %s any more; restored %s instead",
			record.Branch, remote, fallback)
	}

	// A clone can land with nothing checked out at all: it happens whenever the
	// remote's HEAD names a branch that has since been deleted or renamed. An
	// empty directory is not a restored project, so take any branch the remote
	// does have rather than call that a success.
	if !git.HasHead(ctx, dir) {
		branches := git.RemoteBranches(ctx, dir, remote)
		if len(branches) == 0 {
			return fmt.Errorf("%s has no branches; there is nothing to restore", record.RemoteURL)
		}
		if !git.CheckoutBranch(ctx, dir, branches[0]) {
			return fmt.Errorf("cannot check out %s from %s", branches[0], record.RemoteURL)
		}
		a.ui.Warn("%s has no default branch; restored %s", remote, branches[0])
	}

	if record.Commit == "" {
		return nil
	}
	head, err := git.Head(ctx, dir)
	if err != nil || head == record.Commit {
		return nil
	}
	if git.HasCommit(ctx, dir, record.Commit) {
		a.ui.Note("  %s moved on since it was parked; run 'git checkout %s' for the exact parked commit",
			record.Branch, shortSHA(record.Commit))
		return nil
	}
	a.ui.Warn("the commit this project was parked at (%s) is not on %s any more",
		shortSHA(record.Commit), remote)
	return nil
}

// restoreArchive unpacks a project that was archived because it had no remote.
func (a *App) restoreArchive(ctx context.Context, ref project.Ref, record state.Project, force, keep bool) error {
	archivePath := a.Store.Resolve(record.Archive)
	if _, err := os.Stat(archivePath); err != nil {
		return fmt.Errorf("the archive for %s is missing from %s: %w",
			record.Name, archivePath, err)
	}

	existing, err := os.Lstat(ref.Path)
	switch {
	case err == nil && !force:
		return fmt.Errorf("Project already exists at %s.\n\n       Use --force to replace it.", ref.Display())
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	parent := filepath.Dir(ref.Path)

	// Verify before extracting: a corrupt archive must not overwrite anything.
	step := a.ui.Step("Verifying archive")
	if _, err := archive.Verify(ctx, archive.VerifyOptions{
		Path:       archivePath,
		SHA256:     record.SHA256,
		Entries:    record.EntryCount,
		TreeSHA256: record.TreeSHA256,
	}); err != nil {
		step.Fail()
		return err
	}
	step.Done()

	// Extract to a sibling temporary directory, then swap it in with renames so
	// the original location is never left in a half-written state.
	staging := filepath.Join(parent, fmt.Sprintf(".clav-restore-%s", record.ID))
	_ = os.RemoveAll(staging)
	settled := false
	defer func() {
		if !settled {
			_ = os.RemoveAll(staging)
		}
	}()

	step = a.ui.Step("Extracting")
	res, err := archive.Extract(ctx, archive.ExtractOptions{Path: archivePath, Dest: staging})
	if err != nil {
		step.Fail()
		return err
	}
	step.Done()
	a.ui.Detail("%d entries, %s written", res.Entries, HumanSize(res.Bytes))
	for _, w := range res.Warnings {
		a.ui.Warn("%s", w)
	}

	if existing != nil {
		// --force. The directory itself stays — a shell may be standing in it —
		// but everything inside is replaced by the archive's contents. The
		// archive has already been verified and unpacked at this point, so
		// nothing is thrown away for a restore that then fails.
		if err := project.Wipe(ref.Path); err != nil {
			return fmt.Errorf("cannot clear %s: %w", ref.Display(), err)
		}
		if _, err := project.Overlay(staging, ref.Path); err != nil {
			return fmt.Errorf("cannot place the restored project at %s: %w\n"+
				"       the extracted copy is at %s", ref.Display(), err, project.Shorten(staging))
		}
		settled = true
		if err := os.RemoveAll(staging); err != nil {
			a.ui.Warn("could not delete %s: %v", project.Shorten(staging), err)
		}
	} else {
		if err := os.Rename(staging, ref.Path); err != nil {
			return fmt.Errorf("cannot place the restored project at %s: %w", ref.Display(), err)
		}
		settled = true
	}

	if err := a.release(ref, record, keep); err != nil {
		return err
	}
	a.ui.Result("%s restored · %s", a.ui.bold(record.Name), ref.Display())
	return nil
}

// release drops the parked entry, and the archive behind it, once a project is
// back on disk. The state entry goes first, so a crash can leave an unused
// file but never a dangling reference.
func (a *App) release(ref project.Ref, record state.Project, keep bool) error {
	if keep {
		a.ui.Detail("%s is still listed as parked", record.Name)
		return nil
	}
	if err := a.Store.Update(func(f *state.File) error {
		f.Delete(ref.Key)
		return nil
	}); err != nil {
		return fmt.Errorf("the project was restored but clav's state could not be updated: %w", err)
	}
	for _, rel := range []string{record.Archive, record.Rescue} {
		if rel == "" {
			continue
		}
		if err := os.Remove(a.Store.Resolve(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.ui.Warn("could not delete %s: %v", rel, err)
		}
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
