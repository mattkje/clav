package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattkje/clav/internal/git"
	"github.com/mattkje/clav/internal/project"
)

const sweepUsage = `Usage: clav sweep [path] [--older-than 60d] [--push] [--rescue] [--yes] [--dry-run] [--depth N] [--verbose]

Looks through a directory for git projects worth parking, shows what each one
would free, and parks the ones that are ready. A project is ready when the
remote already has everything: no uncommitted changes, no unpushed commits, no
stash.

With no path, clav sweeps the current directory.

  --older-than    only consider projects untouched for this long (default 30d)
  --push          push what the remote is missing, so those projects count too
  --rescue        save stashes and uncommitted changes, so those count too
  --yes           do not ask before parking
  --dry-run       show the table and stop
  --depth N       how far below the path to look for projects (default 4)`

// candidate is one project a sweep found.
type candidate struct {
	ref        project.Ref
	frees      int64
	lastCommit time.Time
	status     string // empty means ready to park
}

func (a *App) sweep(ctx context.Context, args []string) error {
	cmd := newCommand("sweep", sweepUsage)
	olderThan := cmd.fs.String("older-than", "30d", "only consider projects untouched this long")
	yes := cmd.fs.Bool("yes", false, "do not ask before parking")
	dryRun := cmd.fs.Bool("dry-run", false, "show the table and stop")
	depth := cmd.fs.Int("depth", project.DefaultDepth, "how deep to look")
	push := cmd.fs.Bool("push", false, "push what the remote is missing first")
	rescue := cmd.fs.Bool("rescue", false, "save stashes and uncommitted changes")
	if err := cmd.parse(args); err != nil {
		return err
	}
	a.ui.Verbose = *cmd.verbose

	age, err := parseAge(*olderThan)
	if err != nil {
		return usagef("%v\n\n%s", err, sweepUsage)
	}

	dir := ""
	switch cmd.fs.NArg() {
	case 0:
		if dir, err = a.workingDir(); err != nil {
			return err
		}
	case 1:
		dir = cmd.fs.Arg(0)
	default:
		return usagef("expected one path, got %d\n\n%s", cmd.fs.NArg(), sweepUsage)
	}

	repos, err := project.FindRepos(dir, *depth)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		a.ui.Note("no git projects under %s", project.Shorten(dir))
		return nil
	}
	a.ui.Detail("%s found under %s", plural2(len(repos), "project"), project.Shorten(dir))

	cutoff := time.Now().Add(-age)
	candidates, err := a.inspectCandidates(ctx, repos, cutoff, *push, *rescue)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		a.ui.Note("nothing under %s has been quiet for %s", project.Shorten(dir), *olderThan)
		return nil
	}

	ready := a.reportSweep(candidates)
	if len(ready) == 0 || *dryRun {
		return nil
	}
	if !*yes {
		ok, err := a.confirm(fmt.Sprintf("Park %s? [y/N] ", plural2(len(ready), "project")))
		if err != nil {
			return err
		}
		if !ok {
			a.ui.Line("Cancelled.")
			return nil
		}
	}

	// Each project goes through the full park, network checks included: the
	// table was built from what is on this machine, and that is not enough to
	// delete anything on.
	parked, failed := 0, 0
	for _, c := range ready {
		if err := a.parkPath(ctx, c.ref.Path, "", parkOptions{push: *push, rescue: *rescue}, ""); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			a.ui.Warn("%s: %v", c.ref.Name, firstLine(err))
			failed++
			continue
		}
		parked++
	}
	if failed > 0 {
		a.ui.Note("%s parked, %d skipped", plural2(parked, "project"), failed)
	}
	return nil
}

// inspectCandidates asks each project about itself. Every question is answered
// from the local repository, so a sweep of a large directory costs no network
// round trips at all.
func (a *App) inspectCandidates(ctx context.Context, repos []string, cutoff time.Time, push, rescue bool) ([]candidate, error) {
	current, err := a.Store.Load()
	if err != nil {
		return nil, err
	}

	var out []candidate
	for _, path := range repos {
		ref, err := project.Locate(path)
		if err != nil {
			continue
		}
		if _, parked := current.Find(ref.Key); parked {
			a.ui.Detail("%s is already parked", ref.Name)
			continue
		}
		repo, err := git.Open(ctx, path)
		if err != nil {
			continue
		}
		last, lerr := git.LastCommit(ctx, path)
		if lerr == nil && last.After(cutoff) {
			a.ui.Detail("%s was worked on %s", ref.Name, RelTime(last, time.Now()))
			continue
		}

		c := candidate{ref: ref, lastCommit: last, status: a.blocker(ctx, repo, push, rescue)}
		if c.status == "" {
			// Only a project that could actually be parked is worth measuring.
			doomed, derr := a.doomedPaths(ctx, repo, false)
			if derr != nil {
				continue
			}
			purge, merr := project.Measure(ref.Path, doomed)
			if merr != nil {
				continue
			}
			c.frees = purge.Bytes
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].status == "" && out[j].status != "" {
			return true
		}
		if out[i].status != "" && out[j].status == "" {
			return false
		}
		return out[i].frees > out[j].frees
	})
	return out, nil
}

// blocker names the reason a project is not ready, or "" when it is. The
// answers come from the local repository only; park re-checks against the
// remote before deleting anything.
func (a *App) blocker(ctx context.Context, repo *git.Repo, push, rescue bool) string {
	if _, ok := repo.Origin(); !ok {
		return "no remote"
	}
	if repo.Commit == "" {
		return "no commits"
	}
	if dirty, err := repo.DirtyTracked(ctx); err == nil && len(dirty) > 0 && !rescue {
		return plural2(len(dirty), "uncommitted file")
	}
	if n := repo.Stashes(ctx); n > 0 && !rescue {
		return plural2(n, "stash entry")
	}
	if n := repo.UnpushedLocal(ctx); n > 0 && !push {
		return plural2(n, "unpushed commit")
	}
	return ""
}

// reportSweep prints the table and returns the projects that are ready.
func (a *App) reportSweep(candidates []candidate) []candidate {
	var ready []candidate
	var total int64
	rows := make([][4]string, 0, len(candidates))
	now := time.Now()
	for _, c := range candidates {
		frees, status := "—", c.status
		if c.status == "" {
			frees = HumanSize(c.frees)
			status = "ready"
			total += c.frees
			ready = append(ready, c)
		}
		last := "no commits"
		if !c.lastCommit.IsZero() {
			last = RelTime(c.lastCommit, now)
		}
		rows = append(rows, [4]string{c.ref.Name, frees, last, status})
	}

	w := [4]int{len("NAME"), len("FREES"), len("LAST COMMIT"), 0}
	for _, r := range rows {
		for i := 0; i < 3; i++ {
			w[i] = max(w[i], len(r[i]))
		}
	}
	a.ui.Line("%s", a.ui.dim(fmt.Sprintf("%-*s  %-*s  %-*s  %s",
		w[0], "NAME", w[1], "FREES", w[2], "LAST COMMIT", "STATUS")))
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-*s  %s", w[0], r[0], w[1], r[1], w[2], r[2], r[3])
		if r[3] == "ready" {
			a.ui.Line("%s", line)
		} else {
			a.ui.Line("%s", a.ui.dim(line))
		}
	}
	if len(ready) == 0 {
		a.ui.Note("nothing is ready to park")
		return nil
	}
	a.ui.Note("%s ready · %s", plural2(len(ready), "project"), HumanSize(total))
	return ready
}

// parseAge accepts the durations people actually type for this: 30d, 6w, 3mo,
// 1y, as well as anything Go understands.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	units := []struct {
		suffix string
		unit   time.Duration
	}{
		{"mo", 30 * 24 * time.Hour},
		{"y", 365 * 24 * time.Hour},
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
	}
	for _, u := range units {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("cannot read %q as a duration", s)
		}
		return time.Duration(n * float64(u.unit)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("cannot read %q as a duration; try 30d, 6w or 3mo", s)
	}
	return d, nil
}

func firstLine(err error) string {
	if i := strings.IndexByte(err.Error(), '\n'); i >= 0 {
		return err.Error()[:i]
	}
	return err.Error()
}
