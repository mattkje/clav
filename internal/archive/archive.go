// Package archive creates, verifies and extracts clav archives.
//
// An archive is a tar stream compressed with zstd. tar is used because it is
// the only widely available container format that faithfully represents a POSIX
// directory tree: permissions, modification times, symlinks, hard links, empty
// directories, device nodes and arbitrary file names all survive a round trip.
//
// Nothing in this package knows about Git, ignore files or "important" files.
// Everything inside the project directory is archived.
package archive

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Compression identifies the codec written into project metadata.
const Compression = "zstd"

// Extension is the archive file suffix.
const Extension = ".tar.zst"

// RootEntry is the archive entry representing the project directory itself. It
// carries the root's own permissions and modification time.
const RootEntry = "."

// Filter decides which paths are left out of an archive. A nil Filter archives
// everything, which is what clav v1 always does.
type Filter interface {
	Exclude(rel string, isDir bool) bool
}

// Progress is called periodically during long operations.
type Progress func(entries int, bytes int64)

func (p Progress) report(entries int, bytes int64) {
	if p != nil {
		p(entries, bytes)
	}
}

// CreateOptions configures Create.
type CreateOptions struct {
	// Root is the directory to archive. It must be an existing directory.
	Root string
	// Dest is the final archive path. It is only created, via an atomic
	// rename, once the whole archive has been written and flushed.
	Dest string
	// TempDir is where the in-progress archive lives. It must be on the same
	// filesystem as Dest. Defaults to the directory of Dest.
	TempDir string
	// Level selects the zstd effort: 1 fastest .. 4 smallest. 0 means default.
	Level int
	// Filter optionally excludes paths.
	Filter Filter
	// Progress is called as entries are written.
	Progress Progress
}

// Manifest describes a written archive and is everything needed to verify it
// again later.
type Manifest struct {
	Path        string
	Size        int64  // size of the compressed archive
	SHA256      string // checksum of the compressed archive bytes
	Entries     int    // number of tar entries
	TreeSHA256  string // checksum of the logical tree (names, modes, sizes, links)
	TotalBytes  int64  // uncompressed bytes of regular file content
	Warnings    []string
	Compression string
}

// Create writes the contents of opts.Root to opts.Dest.
//
// The archive is written to a temporary file, fsynced, and only then renamed
// into place, so a partially written archive is never visible under its final
// name. If Create returns an error, no temporary file is left behind and Dest
// is untouched.
func Create(ctx context.Context, opts CreateOptions) (*Manifest, error) {
	if opts.Root == "" || opts.Dest == "" {
		return nil, errors.New("archive: Root and Dest are required")
	}
	tempDir := opts.TempDir
	if tempDir == "" {
		tempDir = filepath.Dir(opts.Dest)
	}
	if _, err := os.Stat(tempDir); err != nil {
		return nil, fmt.Errorf("archive: temp dir unusable: %w", err)
	}

	tmp, err := os.CreateTemp(tempDir, ".clav-park-*"+Extension+".part")
	if err != nil {
		return nil, fmt.Errorf("archive: cannot create temporary archive: %w", err)
	}
	tmpName := tmp.Name()

	// Any failure past this point must leave no trace.
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return nil, err
	}

	hasher := sha256.New()
	buffered := bufio.NewWriterSize(tmp, 1<<20)
	sink := io.MultiWriter(buffered, hasher)

	enc, err := zstd.NewWriter(sink, zstd.WithEncoderLevel(encoderLevel(opts.Level)))
	if err != nil {
		return nil, fmt.Errorf("archive: cannot start compressor: %w", err)
	}
	tw := tar.NewWriter(enc)

	w := &writer{
		tw:     tw,
		root:   opts.Root,
		filter: opts.Filter,
		tree:   sha256.New(),
		links:  map[string]string{},
	}
	if err := w.walk(ctx, opts.Progress); err != nil {
		return nil, err
	}

	// Close in order: tar padding, compressor frame, buffer, file. A failure in
	// any of these means the archive is not trustworthy.
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("archive: cannot finalise tar stream: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("archive: cannot finalise compression: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		return nil, fmt.Errorf("archive: cannot flush archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("archive: cannot fsync archive: %w", err)
	}
	fi, err := tmp.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("archive: cannot close archive: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, opts.Dest); err != nil {
		return nil, fmt.Errorf("archive: cannot commit archive: %w", err)
	}
	committed = true
	syncDir(filepath.Dir(opts.Dest))

	return &Manifest{
		Path:        opts.Dest,
		Size:        size,
		SHA256:      hex.EncodeToString(hasher.Sum(nil)),
		Entries:     w.entries,
		TreeSHA256:  hex.EncodeToString(w.tree.Sum(nil)),
		TotalBytes:  w.bytes,
		Warnings:    w.warnings,
		Compression: Compression,
	}, nil
}

type writer struct {
	tw       *tar.Writer
	root     string
	filter   Filter
	tree     hash.Hash
	links    map[string]string // hard-link identity -> first archive name
	entries  int
	bytes    int64
	warnings []string
}

func (w *writer) walk(ctx context.Context, progress Progress) error {
	return filepath.WalkDir(w.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("archive: cannot read %s: %w", p, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		name := RootEntry
		if p != w.root {
			rel, rerr := filepath.Rel(w.root, p)
			if rerr != nil {
				return rerr
			}
			name = filepath.ToSlash(rel)
			if w.filter != nil && w.filter.Exclude(name, d.IsDir()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}

		info, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("archive: cannot stat %s: %w", p, ierr)
		}
		if err := w.add(p, name, info); err != nil {
			return err
		}
		progress.report(w.entries, w.bytes)
		return nil
	})
}

func (w *writer) add(p, name string, info os.FileInfo) error {
	mode := info.Mode()

	// Sockets have no meaningful on-disk representation in tar and cannot be
	// recreated usefully. Skipping one is recorded, never silent.
	if mode&os.ModeSocket != 0 {
		w.warnings = append(w.warnings, fmt.Sprintf("skipped socket %s", name))
		return nil
	}

	link := ""
	if mode&os.ModeSymlink != 0 {
		var err error
		if link, err = os.Readlink(p); err != nil {
			return fmt.Errorf("archive: cannot read symlink %s: %w", p, err)
		}
	}

	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("archive: cannot describe %s: %w", p, err)
	}
	h.Format = tar.FormatPAX
	h.Name = name
	if info.IsDir() && name != RootEntry {
		h.Name = name + "/"
	}
	if name == RootEntry {
		h.Name = RootEntry + "/"
	}

	// Hard links: the first occurrence is stored in full, later ones become
	// link entries so the link structure survives a restore.
	if mode.IsRegular() {
		if key, multi := hardLinkKey(info); multi {
			if first, seen := w.links[key]; seen {
				h.Typeflag = tar.TypeLink
				h.Linkname = first
				h.Size = 0
			} else {
				w.links[key] = h.Name
			}
		}
	}

	if err := w.tw.WriteHeader(h); err != nil {
		return fmt.Errorf("archive: cannot write header for %s: %w", name, err)
	}
	w.entries++
	digestEntry(w.tree, h)

	if h.Typeflag != tar.TypeReg || h.Size == 0 {
		return nil
	}

	f, err := openNoFollow(p)
	if err != nil {
		return fmt.Errorf("archive: cannot open %s: %w", p, err)
	}
	defer f.Close()

	n, err := io.CopyN(w.tw, f, h.Size)
	w.bytes += n
	switch {
	case err == nil || errors.Is(err, io.EOF):
		if n < h.Size {
			// The file shrank while we were reading it. Pad so the tar stream
			// stays well formed, and tell the user which file was racy.
			if _, perr := io.CopyN(w.tw, zeroReader{}, h.Size-n); perr != nil {
				return fmt.Errorf("archive: cannot pad %s: %w", name, perr)
			}
			w.warnings = append(w.warnings,
				fmt.Sprintf("%s shrank while archiving; padded to its original size", name))
		}
	default:
		return fmt.Errorf("archive: cannot read %s: %w", p, err)
	}
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// digestEntry folds a header's identity into the tree checksum. File content is
// covered by the archive checksum; this covers structure, so a restore can be
// checked against what was scanned.
func digestEntry(h hash.Hash, hdr *tar.Header) {
	size := hdr.Size
	if hdr.Typeflag != tar.TypeReg {
		size = 0
	}
	fmt.Fprintf(h, "%s\x00%c\x00%o\x00%d\x00%s\n",
		hdr.Name, hdr.Typeflag, hdr.Mode, size, hdr.Linkname)
}

// VerifyOptions configures Verify.
type VerifyOptions struct {
	Path       string
	SHA256     string // expected checksum of the compressed bytes; optional
	Entries    int    // expected entry count; optional
	TreeSHA256 string // expected tree checksum; optional
	Progress   Progress
}

// VerifyResult reports what verification observed.
type VerifyResult struct {
	Size       int64
	SHA256     string
	Entries    int
	TreeSHA256 string
	TotalBytes int64
}

// Verify reads an archive end to end: it decompresses every byte, walks every
// tar header, reads every file's content, and recomputes both the archive
// checksum and the logical tree checksum. Any mismatch, truncation or
// corruption is an error.
//
// This is what makes deleting the original safe: the archive has been proven
// readable before anything is destroyed.
func Verify(ctx context.Context, opts VerifyOptions) (*VerifyResult, error) {
	f, err := os.Open(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("archive: cannot open %s: %w", opts.Path, err)
	}
	defer f.Close()

	hasher := sha256.New()
	tee := io.TeeReader(bufio.NewReaderSize(f, 1<<20), hasher)

	dec, err := zstd.NewReader(tee)
	if err != nil {
		return nil, fmt.Errorf("archive: cannot read %s: %w", opts.Path, err)
	}
	// The decoder reads ahead from its own goroutines, so it must be shut down
	// before anything else touches the underlying reader.
	closed := false
	defer func() {
		if !closed {
			dec.Close()
		}
	}()

	tr := tar.NewReader(dec)
	tree := sha256.New()
	res := &VerifyResult{}
	sawRoot := false

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %s is corrupt: %w", filepath.Base(opts.Path), err)
		}
		if err := checkName(h.Name); err != nil {
			return nil, fmt.Errorf("archive: %s contains an unsafe path: %w", filepath.Base(opts.Path), err)
		}
		if strings.TrimSuffix(h.Name, "/") == RootEntry {
			sawRoot = true
		}
		res.Entries++
		digestEntry(tree, h)
		if h.Typeflag == tar.TypeReg {
			n, cerr := io.Copy(io.Discard, tr)
			res.TotalBytes += n
			if cerr != nil {
				return nil, fmt.Errorf("archive: %s is corrupt while reading %s: %w",
					filepath.Base(opts.Path), h.Name, cerr)
			}
		}
		opts.Progress.report(res.Entries, res.TotalBytes)
	}

	// Stop the decompressor, then drain any trailing bytes so the checksum
	// covers the whole file and not just the part the decoder consumed.
	dec.Close()
	closed = true
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, fmt.Errorf("archive: cannot read %s: %w", opts.Path, err)
	}
	if fi, err := f.Stat(); err == nil {
		res.Size = fi.Size()
	}
	res.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	res.TreeSHA256 = hex.EncodeToString(tree.Sum(nil))

	if !sawRoot {
		return nil, errors.New("archive: no project root entry found; archive does not contain a project")
	}
	if opts.Entries > 0 && res.Entries != opts.Entries {
		return nil, fmt.Errorf("archive: expected %d entries, found %d", opts.Entries, res.Entries)
	}
	if opts.TreeSHA256 != "" && res.TreeSHA256 != opts.TreeSHA256 {
		return nil, errors.New("archive: contents do not match the recorded project structure")
	}
	if opts.SHA256 != "" && res.SHA256 != opts.SHA256 {
		return nil, errors.New("archive: checksum mismatch; the archive has been modified or corrupted")
	}
	return res, nil
}

// ExtractOptions configures Extract.
type ExtractOptions struct {
	Path string
	// Dest must not exist; Extract creates it. Callers extract into a
	// temporary directory and rename it into place.
	Dest     string
	Progress Progress
}

// ExtractResult reports what was written.
type ExtractResult struct {
	Entries  int
	Bytes    int64
	Warnings []string
}

type dirMeta struct {
	path string
	mode os.FileMode
	hdr  *tar.Header
}

// Extract unpacks an archive into a fresh directory.
//
// Entry names are validated (no absolute paths, no "..") and every parent
// directory is confirmed to be a real directory before anything is written, so
// a hostile or damaged archive cannot escape Dest through a symlink.
func Extract(ctx context.Context, opts ExtractOptions) (*ExtractResult, error) {
	if _, err := os.Lstat(opts.Dest); err == nil {
		return nil, fmt.Errorf("archive: extraction target %s already exists", opts.Dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(opts.Dest, 0o700); err != nil {
		return nil, fmt.Errorf("archive: cannot create %s: %w", opts.Dest, err)
	}

	f, err := os.Open(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("archive: cannot open %s: %w", opts.Path, err)
	}
	defer f.Close()

	dec, err := zstd.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("archive: cannot read %s: %w", opts.Path, err)
	}
	defer dec.Close()

	res := &ExtractResult{}
	var dirs []dirMeta
	type pending struct{ target, name string }
	var hardLinks []pending
	var files []*tar.Header

	tr := tar.NewReader(dec)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %s is corrupt: %w", filepath.Base(opts.Path), err)
		}
		if err := checkName(h.Name); err != nil {
			return nil, fmt.Errorf("archive: refusing unsafe path %q: %w", h.Name, err)
		}
		rel := strings.TrimSuffix(h.Name, "/")
		target, err := resolveInside(opts.Dest, rel)
		if err != nil {
			return nil, err
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if rel != RootEntry {
				if err := safeMkdir(opts.Dest, rel); err != nil {
					return nil, err
				}
			}
			dirs = append(dirs, dirMeta{path: target, mode: h.FileInfo().Mode().Perm(), hdr: h})

		case tar.TypeReg:
			if err := safeMkdir(opts.Dest, path.Dir(rel)); err != nil {
				return nil, err
			}
			n, werr := writeFile(target, h, tr)
			res.Bytes += n
			if werr != nil {
				return nil, werr
			}
			files = append(files, h)

		case tar.TypeSymlink:
			if err := safeMkdir(opts.Dest, path.Dir(rel)); err != nil {
				return nil, err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return nil, fmt.Errorf("archive: cannot recreate symlink %s: %w", rel, err)
			}
			_ = lchtimes(target, h)

		case tar.TypeLink:
			if err := checkName(h.Linkname); err != nil {
				return nil, fmt.Errorf("archive: refusing unsafe hard link %q: %w", h.Linkname, err)
			}
			hardLinks = append(hardLinks, pending{target: target, name: strings.TrimSuffix(h.Linkname, "/")})

		case tar.TypeFifo, tar.TypeChar, tar.TypeBlock:
			if err := safeMkdir(opts.Dest, path.Dir(rel)); err != nil {
				return nil, err
			}
			if err := makeSpecial(target, h); err != nil {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("could not recreate %s (%s): %v", rel, describeType(h.Typeflag), err))
			}

		default:
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("skipped %s: unsupported entry type %q", rel, h.Typeflag))
			continue
		}
		res.Entries++
		opts.Progress.report(res.Entries, res.Bytes)
	}

	// Hard links last: their targets are guaranteed to exist by now.
	for _, hl := range hardLinks {
		src, err := resolveInside(opts.Dest, hl.name)
		if err != nil {
			return nil, err
		}
		if err := os.Link(src, hl.target); err != nil {
			return nil, fmt.Errorf("archive: cannot recreate hard link %s: %w", hl.target, err)
		}
	}

	// File times are applied after content is written; directory permissions
	// and times are applied deepest first so that writing children cannot
	// disturb a parent's timestamps or a read-only mode.
	for _, h := range files {
		target, err := resolveInside(opts.Dest, strings.TrimSuffix(h.Name, "/"))
		if err != nil {
			return nil, err
		}
		_ = os.Chtimes(target, h.AccessTime, h.ModTime)
	}
	sort.SliceStable(dirs, func(i, j int) bool {
		return strings.Count(dirs[i].path, string(os.PathSeparator)) >
			strings.Count(dirs[j].path, string(os.PathSeparator))
	})
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return nil, fmt.Errorf("archive: cannot restore permissions on %s: %w", d.path, err)
		}
		_ = os.Chtimes(d.path, d.hdr.AccessTime, d.hdr.ModTime)
	}
	return res, nil
}

func writeFile(target string, h *tar.Header, r io.Reader) (int64, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL|osNoFollow, h.FileInfo().Mode().Perm())
	if err != nil {
		return 0, fmt.Errorf("archive: cannot create %s: %w", target, err)
	}
	n, cerr := io.Copy(f, r)
	if cerr != nil {
		f.Close()
		return n, fmt.Errorf("archive: cannot write %s: %w", target, cerr)
	}
	if err := f.Chmod(h.FileInfo().Mode().Perm()); err != nil {
		f.Close()
		return n, fmt.Errorf("archive: cannot set permissions on %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return n, fmt.Errorf("archive: cannot close %s: %w", target, err)
	}
	return n, nil
}

// checkName rejects anything that could write outside the destination.
func checkName(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return errors.New("absolute path")
	}
	if filepath.VolumeName(name) != "" {
		return errors.New("volume-qualified path")
	}
	for _, elem := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if elem == ".." {
			return errors.New(`contains ".."`)
		}
	}
	return nil
}

// resolveInside maps an archive-relative name to an absolute path under dest.
func resolveInside(dest, rel string) (string, error) {
	if err := checkName(rel); err != nil {
		return "", fmt.Errorf("archive: unsafe path %q: %w", rel, err)
	}
	if rel == RootEntry {
		return dest, nil
	}
	target := filepath.Join(dest, filepath.FromSlash(rel))
	cleanDest := filepath.Clean(dest)
	if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive: path %q escapes the destination", rel)
	}
	return target, nil
}

// safeMkdir creates rel under dest, refusing to traverse an existing symlink.
func safeMkdir(dest, rel string) error {
	if rel == "" || rel == RootEntry {
		return nil
	}
	cur := dest
	for _, elem := range strings.Split(rel, "/") {
		if elem == "" || elem == RootEntry {
			continue
		}
		cur = filepath.Join(cur, elem)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("archive: cannot create %s: %w", cur, err)
			}
		case err != nil:
			return err
		case fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("archive: refusing to write through symlink %s", cur)
		case !fi.IsDir():
			return fmt.Errorf("archive: %s exists and is not a directory", cur)
		}
	}
	return nil
}

func describeType(t byte) string {
	switch t {
	case tar.TypeFifo:
		return "fifo"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	default:
		return "special file"
	}
}

func encoderLevel(level int) zstd.EncoderLevel {
	switch level {
	case 1:
		return zstd.SpeedFastest
	case 3:
		return zstd.SpeedBetterCompression
	case 4:
		return zstd.SpeedBestCompression
	default:
		return zstd.SpeedDefault
	}
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
