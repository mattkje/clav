# clav

Park a git project when you're done with it. Restore it when you need it again.

`clav park` frees the disk a project is using by deleting everything git can
give back — tracked files, `.git`, and the ignored build and dependency
directories — and leaving everything else exactly where it is. `clav restore`
clones the project again from its remote and puts your kept files back with it.

The rule is simple: **if the remote has it, clav deletes it; if only you have
it, clav keeps it.** Your `.env`, your notes, your editor settings and anything
else git never knew about stay in the folder, untouched.

```
$ clav park ~/Projects/sorta
✓ sorta parked · 1.84 GB freed · 3 files kept

$ ls -a ~/Projects/sorta
.  ..  .env  notes.txt  todo.md

$ clav restore ~/Projects/sorta
✓ sorta restored · ~/Projects/sorta · 3 kept files put back
```

Parking is refused when the remote does not already have your work:

```
$ clav park ~/Projects/sorta
clav: ~/Projects/sorta has 2 commits that origin does not have
         a1b2c3d fix the retry loop
         d4e5f6a wip
       push them, or park anyway with --force
```

## Install

### One line

```bash
curl -fsSL https://raw.githubusercontent.com/mattkje/clav/main/install.sh | sh
```

Works out your platform, downloads that binary from the latest GitHub release,
verifies it against the release checksums, and puts it in `~/.local/bin`. It
refuses to install anything whose checksum does not match.

```bash
CLAV_INSTALL_DIR=/usr/local/bin ...   # install somewhere else (uses sudo if needed)
CLAV_VERSION=v0.2.0 ...               # pin a version
```

Piping a script into a shell is worth reading first:
<https://github.com/mattkje/clav/blob/main/install.sh>.

### From source

Requires Go 1.22 or newer and `git` on your `PATH`. The result is a single
static binary.

```bash
git clone https://github.com/mattkje/clav.git
cd clav
make install            # builds and installs to /usr/local/bin/clav
```

Install somewhere else with `PREFIX`:

```bash
make install PREFIX="$HOME/.local"
```

### With go install

```bash
go install github.com/mattkje/clav/cmd/clav@latest
```

### Build only

```bash
make build              # -> bin/clav
make dist               # -> dist/clav-{darwin,linux}-{amd64,arm64}
make checksums          # -> dist/checksums.txt
```

Tagging a version (`git tag v0.2.0 && git push --tags`) builds all four
binaries in CI and publishes them as a GitHub release, which is what the
installer downloads.

Supported platforms: macOS and Linux, amd64 and arm64.

## Commands

| Command | What it does |
| --- | --- |
| `clav park [path]` | Delete the project's tracked content, keep your own files |
| `clav sweep [path]` | Find projects worth parking under a directory |
| `clav restore [path]` | Clone the project back and put the kept files with it |
| `clav list` | List parked projects |
| `clav inspect <path>` | Show details about a parked project |
| `clav remove <path>` | Forget a parked project |

Flags: `--push`, `--rescue`, `--dry-run`, `--force`, `--keep-ignored`, `--keep`,
`--verbose` / `-v`, `--help` / `-h`, `--version`. Flags may appear before or
after the path.

Output is one line per command. `--verbose` shows every step.

### park

```bash
clav park                                # the project you are in
clav park ~/Projects/sorta               # or name it explicitly
clav park --dry-run                      # show what would go, change nothing
clav park --push                         # push what the remote is missing first
clav park --rescue                       # save stashes and uncommitted work
clav park ~/Projects/sorta --verbose     # show each step
```

`--dry-run` answers the only question that matters before a destructive
command, and touches nothing:

```
$ clav park --dry-run
would delete  312 files          → 1.84 GB freed
would keep    11 files           → 41 KB
✓ sorta dry run · nothing changed
```

`--push` clears the one blocker clav can clear by itself. Every branch the
remote is behind on is pushed — including a branch that never had an upstream,
which is the usual reason work goes missing — and the remote is asked again
before anything is deleted:

```
$ clav park --push
pushed booking-fix to origin
✓ sorta parked · 1.84 GB freed · 11 files kept
```

`--rescue` saves the work that has nowhere else to live. Every stash entry, and
the uncommitted changes — staged and unstaged both — are written into a git
bundle in `~/.clav/rescue`, and `clav restore` puts them straight back on the
stash:

```
$ clav park --rescue
✓ sorta parked · 1.84 GB freed · 11 files kept · 3 stash entries rescued

$ clav restore
✓ sorta restored · ~/Projects/sorta · 11 files kept in place · 3 entries back on the stash
  see them with 'git stash list'; 'git stash pop --index' keeps what was staged

$ git stash list
stash@{0}: clav rescue: uncommitted changes when parked
stash@{1}: On main: second idea
stash@{2}: On main: first idea
```

What was staged stays staged: the entries are real stash commits, index state
and all, so `git stash pop --index` gives back exactly the working copy you
parked. Nothing is pushed anywhere — this is work you never chose to publish,
so clav keeps it locally. The bundle holds only what the remote does not already have,
so it is usually a few kilobytes, and it is deleted once the project is
restored.

#### What is deleted, what is kept

| | |
| --- | --- |
| Tracked files | **deleted** — the remote has them |
| `.git/` | **deleted** — restore clones it again |
| Ignored build and dependency directories | **deleted** — a build recreates them |
| Untracked files | **kept** |
| Ignored files that are not build output | **kept** |
| Submodule working copies | **deleted** — restored with the clone |

The deleted ignored directories are matched by name, and only when git already
ignores them:

```
node_modules  bower_components  Pods  .pnpm-store  .yarn  .venv  venv
__pycache__  .pytest_cache  .mypy_cache  .ruff_cache  .tox  .ipynb_checkpoints
target  build  dist  out  obj  bin  vendor  .next  .nuxt  .svelte-kit  .astro
.docusaurus  .turbo  .parcel-cache  .cache  .gradle  .dart_tool  .terraform
.serverless  coverage  .nyc_output  DerivedData  .stack-work  _build  deps
elm-stuff
```

Anything else git ignores — `.env`, `.envrc`, `.idea/`, local config, a scratch
directory you never committed — is kept. `--keep-ignored` keeps the build
directories too.

If nothing is left to keep, the project folder itself is removed.

#### What park refuses

Without `--force`, `clav park` stops when anything would be lost:

- uncommitted changes to tracked files — `--rescue` saves them instead
- commits the remote does not have, on any branch, upstream or not — `--push`
  sends them instead
- a stash: it lives only in `.git` — `--rescue` saves it instead
- a remote it cannot reach, so it cannot confirm the copy exists
- no commit in common with the remote (run `git fetch` first)

`--force` skips these checks but still says what it is about to destroy — dirty
files, stash entries, and commits no remote-tracking branch has — because those
are gone for good once `.git` is deleted.

Branches whose work has already landed are not counted. A forge that
squash-merges leaves every merged branch looking permanently unpushed — its
commits exist nowhere on the remote by name — and a repository anyone has
worked in for a year accumulates dozens of them. clav treats a branch as landed
when it is an ancestor of the remote's default branch, an ancestor of the
remote branch of the same name, or when the single commit a squash-merge of it
would produce is already upstream. Run with `--verbose` to see which branches
were passed over.

The check is a real `git ls-remote` against the remote, not a remote-tracking
ref that may be weeks stale. It gives up after 20 seconds rather than hanging on
a host that is down (`CLAV_REMOTE_TIMEOUT=60s` to wait longer), and never stops
to ask for credentials. `--force` overrides all of it.

`clav park` also refuses a directory that is not a git repository, and a
directory that is only *part* of one:

```
$ clav park ~/Projects/sorta/internal
clav: ~/Projects/sorta/internal is inside a git repository, not the root of one
       park the repository instead: clav park ~/Projects/sorta
```

#### Repositories with no remote

A repository with no remote has nowhere to be cloned back from, so clav archives
the whole directory instead — every file, tracked or not — into
`~/.clav/archives`, verifies the archive byte for byte, and only then deletes
the original:

```
$ clav park ~/Projects/experiment
✓ experiment parked · 84 MB → 12 MB · no remote, archived
```

That is the only case where clav stores your files.

#### Choosing the project

With no path, `clav park` parks the project you are standing in. It walks up
from the current directory looking for a project root and takes the nearest one:

```
.clav-root  .git  .hg  .svn  .jj  go.mod  Cargo.toml  package.json  deno.json
pyproject.toml  composer.json  Gemfile  mix.exs  pubspec.yaml  build.sbt
pom.xml  build.gradle  build.gradle.kts  flake.nix
```

So `cd ~/Projects/sorta/internal/api && clav park` parks all of
`~/Projects/sorta`. The search stops at your home directory, so a dotfiles repo
in `~` never turns your home directory into a project. Run with `--verbose` to
see how the target was chosen.

#### The order of operations

1. Resolve the path and confirm it is a repository root.
2. Refuse if anything local is missing from the remote (unless `--force`).
3. Record the remote, branch and commit in `~/.clav/state.json`.
4. **Only then** delete the tracked content.

The record is written before anything is deleted, so an interrupted park leaves
a restorable entry rather than a half-emptied folder.

### sweep

```bash
clav sweep ~/Developer                       # what could be parked here?
clav sweep ~/Developer --older-than 60d      # only projects idle that long
clav sweep ~/Developer --push                # push stragglers so they qualify
clav sweep ~/Developer --rescue              # save their stashes so they qualify
clav sweep ~/Developer --yes                 # do not ask
clav sweep ~/Developer --dry-run             # table only
```

`sweep` looks for git projects under a directory, works out what each would
free, and parks the ones the remote already has in full:

```
$ clav sweep ~/Developer --older-than 60d
NAME                 FREES    LAST COMMIT   STATUS
song-guess-game      1.2 GB   5 months ago  ready
oms-integration-hub  40 MB    3 months ago  ready
api-gateway          —        2 weeks ago   2 unpushed commits
scratch              —        8 months ago  no remote
2 projects ready · 1.24 GB
Park 2 projects? [y/N]
```

Everything in that table is worked out from the local repositories, so a sweep
of a large directory costs no network calls at all. The projects you then agree
to park each go through the full `clav park`, remote check included: the table
decides what to *offer*, never what to delete.

Projects that are already parked, still being worked on, or have no remote are
left out of the offer. `--depth` (default 4) bounds how far below the path clav
looks; a repository is never descended into, so a submodule or a vendored
checkout inside one is part of that project rather than a project of its own.

### restore

```bash
clav restore                             # the parked project you are standing in
clav restore ~/Projects/sorta            # or name it
clav restore ~/Projects/sorta --force    # replace a repository that is there now
clav restore ~/Projects/sorta --keep     # restore but stay listed as parked
```

With no path, `clav restore` restores the project you are in. Parking leaves the
folder behind whenever there was anything to keep, so `cd sorta && clav restore`
is the natural way to ask for it back; clav also walks up from a subdirectory,
and still resolves the right project from a directory park removed entirely.

Given a path, `<path>` is the project's *original* location — the same path you
parked, which is also exactly where the project is put back. Restore never
resolves against your current directory: the destination comes from
`original_path` in `state.json`. `clav list` shows the paths.

The project is cloned into a temporary sibling directory and then merged into
the folder park left behind — the same directory, kept in place, so a shell
standing in it stays valid and you can park and restore all day from the same
prompt. If a kept file has the same path as a file the repository now has, the
repository's copy wins and yours is kept beside it as `<name>.clav-kept` — both
survive, and clav says so.

If the branch has moved on since you parked, clav restores the branch and points
at the commit you had:

```
main moved on since it was parked; run 'git checkout a1b2c3d4' for the exact parked commit
```

If the branch is not on the remote at all — it was never pushed, or it was
deleted after merging — the project is still restored on the remote's default
branch, and clav says so:

```
warning: booking-fix is not on origin any more; restored main instead
```

`restore` refuses to replace a directory that already holds a repository; pass
`--force` for that, which removes the repository in the way and merges the
kept files as usual. A project that was archived (no remote) refuses any
existing directory unless forced, and is verified in full before a single byte
is extracted.

By default a successful restore releases the entry: the project disappears from
`clav list`, and for an archived project the archive is deleted. `--keep` leaves
the entry in place.

### list

```bash
$ clav list
NAME       FREED   PARKED        LOCATION
sorta      1.84 GB 2 days ago    ~/Projects/sorta
old-api    84 MB   3 weeks ago   ~/Projects/old-api
minecraft  1.2 GB  2 months ago  ~/Projects/minecraft  (archived)
3 projects · 3.12 GB freed
```

`(archived)` marks a project that had no remote and was archived whole.
`(archive missing)` marks one whose archive file has gone.

### inspect

```bash
$ clav inspect ~/Projects/sorta
Project    sorta
Path       ~/Projects/sorta
Parked     2026-08-20 08:30  (2 days ago)
Remote     git@github.com:me/sorta.git  (origin)
Commit     a1b2c3d4 on main
Freed      1.84 GB
Kept       3 files in place
```

For an archived project, `inspect` shows the archive instead, and
`--verify` reads it back end to end and confirms its checksum.

### remove

`remove` forgets a parked project.

```bash
$ clav remove ~/Projects/sorta
This forgets sorta. The code stays on origin and kept files stay in the folder.
Remove sorta? [y/N]
```

For a project parked to its remote this deletes nothing at all — it only drops
clav's entry, though any work `--rescue` saved goes with it, and `remove` says
so before asking. For an archived project it deletes the archive permanently, which
is the only copy. Only `y` or `yes` counts as consent; anything else, including
end-of-input on a non-interactive stdin, is a no. `--force` skips the prompt.

## Storage

```
~/.clav/
├── state.json
├── lock
├── archives/          # only for repositories with no remote
│   └── 4c9a02f5e18d-003.tar.zst
├── rescue/            # stashes and uncommitted work saved by --rescue
│   └── 11ed803aa8b7-001.bundle
└── tmp/
```

Set `CLAV_HOME` to put this somewhere else:

```bash
export CLAV_HOME=/Volumes/Archive/clav
```

`state.json` records how each project gets back:

```json
{
  "version": 2,
  "projects": [
    {
      "id": "11ed803aa8b7-001",
      "kind": "remote",
      "name": "sorta",
      "original_path": "/Users/example/Projects/sorta",
      "path_key": "11ed803aa8b7d888",
      "created_at": "2026-08-20T08:30:00Z",
      "remote_name": "origin",
      "remote_url": "git@github.com:me/sorta.git",
      "branch": "main",
      "commit": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
      "kept_files": 3,
      "freed_bytes": 1842391040,
      "cycle": 1
    }
  ],
  "counters": { "11ed803aa8b7d888": 1 }
}
```

A project with `"kind": "archive"` carries the archive's path, checksum, entry
count and sizes instead.

Every state change takes an exclusive `flock` on `~/.clav/lock`, reloads from
disk, and is written via a temp file + fsync + rename. An interrupted operation
cannot produce a torn state file.

Archives, when clav writes one, are tar streams compressed with
[zstd](https://facebook.github.io/zstd/) — ordinary `.tar.zst` files, readable
with `zstd -d | tar -tvf -` if you ever want at one without `clav`. tar is used
because it is the only widely available format that faithfully represents a
POSIX tree: permissions, timestamps, symlinks, hard links, empty directories and
awkward filenames all survive the round trip.

## Safety

- **`--dry-run` shows the exact delete list** any park would use — the same
  code path, so the number you are shown is the number that happens.
- **Nothing is deleted before the remote is confirmed to have it.** The check is
  a live `ls-remote`, and it covers every local branch and tag, not just the
  current one — minus the branches whose work has demonstrably already landed
  on the remote.
- **Nothing untracked is ever deleted**, apart from ignored directories on the
  regenerable list — and `--keep-ignored` turns even that off.
- **The record is written first.** Park writes its state entry before deleting
  anything; restore releases the entry only after the project is back on disk.
- **Restore builds the result before touching anything.** The clone (or the
  archive extraction) completes in a temporary sibling directory first; only
  then is it merged into place. A kept file that collides with a repository
  file is preserved as `.clav-kept` rather than dropped.
- **A repository with no remote is archived, verified and only then deleted.**
  Verification decompresses every byte, walks every entry and recomputes both
  the archive checksum and a checksum of the tree structure.
- `clav` refuses to park your home directory, the filesystem root, a symlink, or
  anything overlapping its own storage.

## Project identity

A project is identified by its canonical path, not its name, so
`~/Projects/foo` and `~/Work/foo` are different projects that can both be
parked. The key is a hash of the resolved absolute path, stable across
park/restore cycles.

## Tests

```bash
make test               # go test ./...
make race               # go test -race -count=1 ./...
make vet                # go vet + gofmt check
```

The CLI tests drive real commands against real repositories with real bare
remotes, and assert on what survives on disk: that tracked files go, that
`.env` and untracked notes stay, that a dirty or unpushed repository is refused
and left untouched, that a restored project is a working repository on the
branch it was parked from. Tests that need `git` skip when it is missing.

## Layout

```
clav/
├── cmd/clav/           # main(): argument dispatch only
├── internal/
│   ├── git/            # what git says about a repository, and cloning it back
│   ├── archive/        # tar+zstd create / verify / extract (no-remote fallback)
│   ├── project/        # path resolution, identity, deletion and merge policy
│   ├── state/          # ~/.clav/state.json, atomic and locked
│   └── cli/            # commands and output formatting
├── go.mod
├── Makefile
└── README.md
```

The layers do not know about each other's concerns: `git` knows nothing about
parking, `state` knows nothing about what a record means, and `project` knows
nothing about either.

## Not implemented yet

Deliberately out of scope, but the design leaves room for them: `clav status`,
`clav clean`, `clav config`, a configurable list of regenerable directories, and
parking a repository to a remote it does not have yet.

