package cli

import (
	"testing"
	"time"
)

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{999, "999 B"},
		{1000, "1 KB"},
		{1536, "1.54 KB"},
		{84_000_000, "84 MB"},
		{342_000_000, "342 MB"},
		{358_219_120, "358 MB"},
		{1_200_000_000, "1.2 GB"},
		{1_630_000_000, "1.63 GB"},
		{1_842_391_040, "1.84 GB"},
		{2_500_000_000_000, "2.5 TB"},
	}
	for _, c := range cases {
		if got := HumanSize(c.in); got != c.want {
			t.Errorf("HumanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{5 * time.Second, "just now"},
		{90 * time.Second, "1 minute ago"},
		{45 * time.Minute, "45 minutes ago"},
		{3 * time.Hour, "3 hours ago"},
		{48 * time.Hour, "2 days ago"},
		{21 * 24 * time.Hour, "3 weeks ago"},
		{62 * 24 * time.Hour, "2 months ago"},
		{800 * 24 * time.Hour, "2 years ago"},
	}
	for _, c := range cases {
		if got := RelTime(now.Add(-c.ago), now); got != c.want {
			t.Errorf("RelTime(-%v) = %q, want %q", c.ago, got, c.want)
		}
	}
	if got := RelTime(now.Add(time.Hour), now); got != "just now" {
		t.Errorf("a future timestamp should read as %q, got %q", "just now", got)
	}
}

func TestPermuteMovesFlagsFirst(t *testing.T) {
	cmd := newCommand("test", "usage")
	cmd.fs.String("older-than", "", "a flag with a value")
	got := cmd.permute([]string{"/tmp/x", "--force", "-v"})
	want := []string{"--force", "-v", "/tmp/x"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permute = %v, want %v", got, want)
		}
	}
	// A flag that takes a value keeps its value beside it.
	got = cmd.permute([]string{"/tmp/x", "--older-than", "60d", "-v"})
	want = []string{"--older-than", "60d", "-v", "/tmp/x"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permute with a value flag = %v, want %v", got, want)
		}
	}

	// A "--" terminator must still protect a path that looks like a flag.
	got = cmd.permute([]string{"--force", "--", "--weird-dir"})
	if len(got) != 3 || got[0] != "--force" || got[1] != "--" || got[2] != "--weird-dir" {
		t.Fatalf("permute with terminator = %v", got)
	}
}
