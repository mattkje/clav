package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"strings"

	"clav/internal/project"
	"clav/internal/state"
)

const listUsage = `Usage: clav list [--verbose]

Lists parked projects, newest first.`

func (a *App) list(_ context.Context, args []string) error {
	cmd := newCommand("list", listUsage)
	if err := cmd.parse(args); err != nil {
		return err
	}
	if cmd.fs.NArg() > 0 {
		return usagef("list takes no arguments\n\n%s", listUsage)
	}
	a.ui.Verbose = *cmd.verbose

	f, err := a.Store.Load()
	if err != nil {
		return err
	}
	projects := f.Sorted()
	if len(projects) == 0 {
		a.ui.Note("no parked projects · park one with: clav park <path>")
		return nil
	}

	now := time.Now()
	type row struct{ name, size, saved, location, note string }
	rows := make([]row, 0, len(projects))
	var total int64
	for _, p := range projects {
		note := ""
		if p.KindOf() == state.KindArchive {
			note = "  (archived)"
			if _, err := os.Stat(a.Store.Resolve(p.Archive)); err != nil {
				note = "  (archive missing)"
			}
		}
		size := p.FreedBytes
		if size == 0 {
			size = p.ArchiveSize
		}
		rows = append(rows, row{
			name:     p.Name,
			size:     HumanSize(size),
			saved:    RelTime(p.CreatedAt, now),
			location: project.Shorten(p.OriginalPath),
			note:     note,
		})
		total += size
	}

	w := []int{len("NAME"), len("FREED"), len("PARKED")}
	for _, r := range rows {
		w[0] = max(w[0], len(r.name))
		w[1] = max(w[1], len(r.size))
		w[2] = max(w[2], len(r.saved))
	}

	a.ui.Line("%s", a.ui.dim(fmt.Sprintf("%-*s  %-*s  %-*s  %s",
		w[0], "NAME", w[1], "FREED", w[2], "PARKED", "LOCATION")))
	for _, r := range rows {
		a.ui.Line("%-*s  %-*s  %-*s  %s%s",
			w[0], r.name, w[1], r.size, w[2], r.saved, r.location, r.note)
	}
	a.ui.Note("%s · %s freed", plural2(len(rows), "project"), HumanSize(total))
	return nil
}

func plural2(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	if strings.HasSuffix(unit, "y") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(unit, "y"))
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
