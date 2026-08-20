package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/mattkje/clav/internal/archive"
	"github.com/mattkje/clav/internal/project"
	"github.com/mattkje/clav/internal/state"
)

const inspectUsage = `Usage: clav inspect <path> [--verify] [--verbose]

Shows details about a parked project.

  --verify   read the archive end to end and confirm its checksum`

func (a *App) inspect(ctx context.Context, args []string) error {
	cmd := newCommand("inspect", inspectUsage)
	verify := cmd.fs.Bool("verify", false, "verify the archive")
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

	if p.KindOf() == state.KindRemote {
		a.ui.Line("Project    %s", p.Name)
		a.ui.Line("Path       %s", project.Shorten(p.OriginalPath))
		a.ui.Line("Parked     %s  (%s)", p.CreatedAt.Local().Format("2006-01-02 15:04"), RelTime(p.CreatedAt, nowFunc()))
		a.ui.Line("Remote     %s  (%s)", p.RemoteURL, p.RemoteName)
		branch := p.Branch
		if branch == "" {
			branch = "detached"
		}
		a.ui.Line("Commit     %s on %s", shortSHA(p.Commit), branch)
		a.ui.Line("Freed      %s", HumanSize(p.FreedBytes))
		if p.KeptFiles > 0 {
			a.ui.Line("Kept       %s in place", plural2(p.KeptFiles, "file"))
		}
		if n := len(p.RescueMessages); n > 0 {
			a.ui.Line("Rescued    %s, in %s", plural2(n, "stash entry"), p.Rescue)
			for _, m := range p.RescueMessages {
				a.ui.Detail("%s", m)
			}
		}
		a.ui.Detail("id %s, park cycle %d, clav %s", p.ID, p.Cycle, p.ClavVersion)
		return nil
	}

	archivePath := a.Store.Resolve(p.Archive)
	ratio := ""
	if p.OriginalSize > 0 {
		if pct := 100 * (1 - float64(p.ArchiveSize)/float64(p.OriginalSize)); pct >= 1 {
			ratio = fmt.Sprintf("  (%.0f%% smaller)", pct)
		} else {
			ratio = "  (already compressed)"
		}
	}

	a.ui.Line("Project    %s", p.Name)
	a.ui.Line("Path       %s", project.Shorten(p.OriginalPath))
	a.ui.Line("Parked     %s  (%s)", p.CreatedAt.Local().Format("2006-01-02 15:04"), RelTime(p.CreatedAt, nowFunc()))
	a.ui.Line("Kind       archive (the project had no remote)")
	a.ui.Line("Size       %s → %s%s", HumanSize(p.OriginalSize), HumanSize(p.ArchiveSize), ratio)
	a.ui.Line("Archive    %s", project.Shorten(archivePath))
	a.ui.Detail("sha256 %s, %d entries, %s, id %s, park cycle %d", p.SHA256, p.EntryCount, p.Compression, p.ID, p.Cycle)

	if _, err := os.Stat(archivePath); err != nil {
		return fmt.Errorf("the archive is missing: %w", err)
	}

	if *verify {
		step := a.ui.Step("Verifying archive")
		if _, err := archive.Verify(ctx, archive.VerifyOptions{
			Path:       archivePath,
			SHA256:     p.SHA256,
			Entries:    p.EntryCount,
			TreeSHA256: p.TreeSHA256,
		}); err != nil {
			step.Fail()
			return err
		}
		step.Done()
		a.ui.Result("archive verified")
	}
	return nil
}
