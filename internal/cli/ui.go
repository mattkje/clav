package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// UI is clav's terminal output. Normal output is one line per command; the
// running commentary only appears under --verbose.
type UI struct {
	Out     io.Writer
	Err     io.Writer
	Color   bool
	Verbose bool
}

const labelWidth = 26

// nowFunc is indirected so tests can pin relative-time output.
var nowFunc = time.Now

func newUI(out, errw io.Writer) *UI {
	return &UI{Out: out, Err: errw, Color: colorEnabled(out)}
}

func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (u *UI) paint(code, s string) string {
	if !u.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *UI) green(s string) string { return u.paint("32", s) }
func (u *UI) red(s string) string   { return u.paint("31", s) }
func (u *UI) dim(s string) string   { return u.paint("2", s) }
func (u *UI) bold(s string) string  { return u.paint("1", s) }

// Result prints a command's single line of normal output.
func (u *UI) Result(format string, args ...any) {
	fmt.Fprintf(u.Out, u.green("\u2713")+" "+format+"\n", args...)
}

// Note prints a dim advisory line.
func (u *UI) Note(format string, args ...any) {
	fmt.Fprintln(u.Out, u.dim(fmt.Sprintf(format, args...)))
}

// Line prints one line of normal output.
func (u *UI) Line(format string, args ...any) {
	fmt.Fprintf(u.Out, format+"\n", args...)
}

// Blank prints an empty line.
func (u *UI) Blank() { fmt.Fprintln(u.Out) }

// Detail prints an indented line only in verbose mode.
func (u *UI) Detail(format string, args ...any) {
	if u.Verbose {
		fmt.Fprintf(u.Out, "    "+u.dim(format)+"\n", args...)
	}
}

// Warn prints a warning to stderr.
func (u *UI) Warn(format string, args ...any) {
	fmt.Fprintf(u.Err, "warning: "+format+"\n", args...)
}

// Step is one labelled unit of work in a command's output.
type Step struct {
	ui    *UI
	quiet bool
}

// Step prints a label under --verbose and returns a handle for marking the
// outcome. In normal output a step prints nothing: a command that worked says
// so once, at the end.
func (u *UI) Step(label string) *Step {
	if !u.Verbose {
		return &Step{ui: u, quiet: true}
	}
	pad := labelWidth - len([]rune(label))
	if pad < 1 {
		pad = 1
	}
	fmt.Fprintf(u.Out, "  %s%s", label, strings.Repeat(" ", pad))
	return &Step{ui: u}
}

// Done marks a step successful.
func (s *Step) Done() {
	if !s.quiet {
		fmt.Fprintln(s.ui.Out, s.ui.green("✓"))
	}
}

// Fail marks a step failed.
func (s *Step) Fail() {
	if !s.quiet {
		fmt.Fprintln(s.ui.Out, s.ui.red("✗"))
	}
}

// HumanSize renders a byte count with three significant digits and decimal
// units, which is how developers read disk sizes.
func HumanSize(n int64) string {
	if n < 0 {
		return "0 B"
	}
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	f := float64(n)
	i := -1
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	var s string
	switch {
	case f >= 100:
		s = strconv.FormatFloat(math.Round(f), 'f', 0, 64)
	case f >= 10:
		s = trimZeros(strconv.FormatFloat(f, 'f', 1, 64))
	default:
		s = trimZeros(strconv.FormatFloat(f, 'f', 2, 64))
	}
	return s + " " + units[i]
}

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// RelTime renders a timestamp as a short relative age.
func RelTime(t time.Time, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 7*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 31*24*time.Hour:
		return plural(int(d.Hours()/(24*7)), "week")
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/(24*30)), "month")
	default:
		return plural(int(d.Hours()/(24*365)), "year")
	}
}

func plural(n int, unit string) string {
	if n <= 1 {
		n = 1
	}
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}
