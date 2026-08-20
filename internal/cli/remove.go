package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"clav/internal/project"
	"clav/internal/state"
)

const removeUsage = `Usage: clav remove <path> [--force] [--verbose]

Forgets a parked project. For a project parked to its remote this only drops
clav's entry; the code is still on the remote and the kept files are still in
the folder. For an archived project the archive is deleted permanently.

  --force   do not ask for confirmation`

func (a *App) remove(_ context.Context, args []string) error {
	cmd := newCommand("remove", removeUsage)
	force := cmd.fs.Bool("force", false, "skip confirmation")
	if err := cmd.parse(args); err != nil {
		return err
	}
	arg, err := cmd.one("project path")
	if err != nil {
		return err
	}
	a.ui.Verbose = *cmd.verbose

	ref, err := project.Locate(arg)
	if err != nil {
		return err
	}
	f, err := a.Store.Load()
	if err != nil {
		return err
	}
	p, ok := f.Find(ref.Key)
	if !ok {
		return fmt.Errorf("no parked project at %s\n       run 'clav list' to see what is parked", ref.Display())
	}

	archived := p.KindOf() == state.KindArchive
	if !*force {
		if archived {
			a.ui.Line("This permanently deletes the only copy of %s (%s, parked %s).",
				p.Name, HumanSize(p.ArchiveSize), RelTime(p.CreatedAt, nowFunc()))
		} else {
			a.ui.Line("This forgets %s. The code stays on %s and kept files stay in the folder.",
				p.Name, p.RemoteName)
		}
		ok, err := a.confirm(fmt.Sprintf("Remove %s? [y/N] ", p.Name))
		if err != nil {
			return err
		}
		if !ok {
			a.ui.Line("Cancelled.")
			return nil
		}
	}

	// Drop the metadata first: a crash between the two steps leaves an unused
	// file, never a state entry pointing at an archive that is gone.
	if err := a.Store.Update(func(sf *state.File) error {
		if !sf.Delete(ref.Key) {
			return fmt.Errorf("no parked project at %s", ref.Display())
		}
		return nil
	}); err != nil {
		return err
	}
	if p.Archive != "" {
		archivePath := a.Store.Resolve(p.Archive)
		if err := os.Remove(archivePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removed the metadata but could not delete %s: %w", archivePath, err)
		}
	}

	if archived {
		a.ui.Result("%s removed · %s freed", p.Name, HumanSize(p.ArchiveSize))
	} else {
		a.ui.Result("%s forgotten · nothing was deleted", p.Name)
	}
	return nil
}

// confirm asks a yes/no question. Anything other than an explicit yes is a no,
// and a non-interactive stdin never counts as consent.
func (a *App) confirm(prompt string) (bool, error) {
	if a.In == nil {
		return false, errors.New("cannot ask for confirmation; use --force")
	}
	fmt.Fprint(a.Out, prompt)
	reader := bufio.NewReader(a.In)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		// EOF with no input: treat as "no".
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
