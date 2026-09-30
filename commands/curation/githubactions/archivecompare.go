package githubactions

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
)

const (
	archiveReadBufferSize = 1 << 20
	compareChunkSize      = 64 << 10
)

// linksArriveAsText reports whether this runner unzips a symlink into a file holding the link path,
// as Windows runners do (F5). A variable so tests can run both behaviours on any OS.
var linksArriveAsText = runtime.GOOS == "windows"

// ArchiveComparison is what one streamed pass over a downloaded action archive found.
type ArchiveComparison struct {
	Identical bool
	// FirstDifference is the archive path (below the stripped top dir) that is missing or differs on
	// the runner; "" when Identical.
	FirstDifference string
	// Missing reports that FirstDifference is absent on the runner rather than different there.
	Missing bool
	// PaxSHA is the pax global header's comment when it is a full object id, else "".
	PaxSHA string
}

// CompareArchive reports whether runnerDir - the runner's copy of an action - holds the content of
// the tar.gz archive at archivePath, streaming the archive without extracting it.
//
// Only the archive's entries are checked: files the runner or the action added to its own directory
// are expected and ignored. The pass stops at the first entry that is missing or differs, since the
// verdict is known by then. A symlink entry matches the forms a runner leaves one in: a symlink with
// the same target; on Windows, which unzips it so, a file holding the link path; elsewhere, where
// the target is copied, the target's bytes or - for a link to a directory - a tree equal to the
// runner's copy of that directory. A match through the runner's copy of the target counts only when
// that target is itself an archive entry the runner matched, and the link text is never resolved to
// a path outside the archive.
//
// An archive that is unreadable, holds no file or symlink, or has an entry outside its single
// top-level directory is an error rather than a verdict.
func CompareArchive(archivePath, runnerDir string) (comparison ArchiveComparison, err error) {
	root, err := filepath.EvalSymlinks(runnerDir)
	if err != nil {
		return ArchiveComparison{}, fmt.Errorf("resolving the runner's action directory %q: %w", runnerDir, err)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return ArchiveComparison{}, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	gz, err := gzip.NewReader(bufio.NewReaderSize(file, archiveReadBufferSize))
	if err != nil {
		return ArchiveComparison{}, fmt.Errorf("reading the action archive: %w", err)
	}
	defer func() {
		if closeErr := gz.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	archive := tar.NewReader(gz)
	var top string
	compared := 0
	// matched holds every entry found equal on the runner; anchors are the symlinks whose match
	// relied on the runner's copy of their target, checked against matched once the stream ends.
	matched := map[string]bool{}
	var anchors []linkAnchor
	for {
		hdr, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ArchiveComparison{}, fmt.Errorf("reading the action archive: %w", err)
		}
		// Go's reader returns the global header as an entry of its own; it names no content.
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			if sha := hdr.PAXRecords["comment"]; isFullObjectID(sha) {
				comparison.PaxSHA = strings.ToLower(sha)
			}
			continue
		}
		rel, err := stripTopDir(hdr, &top)
		if err != nil {
			return ArchiveComparison{}, err
		}
		if rel == "" {
			continue
		}
		var same, missing bool
		var anchor *linkAnchor
		switch hdr.Typeflag {
		case tar.TypeReg:
			same, missing, err = compareRegularEntry(archive, hdr.Size, runnerPath(root, rel))
		case tar.TypeSymlink:
			same, missing, anchor, err = compareSymlinkEntry(root, rel, hdr.Linkname)
		default:
			continue
		}
		if err != nil {
			return ArchiveComparison{}, fmt.Errorf("comparing %q with the runner's copy: %w", rel, err)
		}
		compared++
		if !same {
			comparison.FirstDifference, comparison.Missing = rel, missing
			return comparison, nil
		}
		matched[rel] = true
		if anchor != nil {
			anchors = append(anchors, *anchor)
		}
	}
	if compared == 0 {
		return ArchiveComparison{}, errors.New("the action archive holds no content")
	}
	for _, anchor := range anchors {
		if !anchor.heldBy(matched) {
			comparison.FirstDifference = anchor.link
			return comparison, nil
		}
	}
	comparison.Identical = true
	return comparison, nil
}

// stripTopDir returns hdr's name below the archive's single top-level directory, recording that
// directory in top on first sight, or "" for the top directory itself.
func stripTopDir(hdr *tar.Header, top *string) (string, error) {
	first, rel, _ := strings.Cut(strings.TrimSuffix(hdr.Name, "/"), "/")
	if first == "" {
		return "", fmt.Errorf("the action archive has unsafe entry %q", hdr.Name)
	}
	if rel == "" && hdr.Typeflag != tar.TypeDir {
		return "", fmt.Errorf("the action archive has entry %q outside its top-level directory", hdr.Name)
	}
	if *top == "" {
		*top = first
	} else if first != *top {
		return "", fmt.Errorf("the action archive has entry %q outside its top-level directory %q", hdr.Name, *top)
	}
	if rel == "" {
		return "", nil
	}
	// IsLocal on the OS form: a backslash is a separator on Windows and must not climb out there.
	if slices.Contains(strings.Split(rel, "/"), "..") || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("the action archive has unsafe entry %q", hdr.Name)
	}
	return rel, nil
}

func runnerPath(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// isMissing reports a path that does not exist, including one whose parent is a file.
func isMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// compareRegularEntry compares the next size bytes of archive with the file at path.
func compareRegularEntry(archive io.Reader, size int64, path string) (same, missing bool, err error) {
	info, err := os.Stat(path)
	if isMissing(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return false, false, nil
	}
	same, err = equalToFile(archive, size, path)
	return same, false, err
}

// equalToFile reports whether the next size bytes of want equal the file at path. want must hold
// size bytes, so its running short is an error; the file running short means they differ.
func equalToFile(want io.Reader, size int64, path string) (same bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	wantBuf, gotBuf := make([]byte, compareChunkSize), make([]byte, compareChunkSize)
	for remaining := size; remaining > 0; {
		n := min(remaining, compareChunkSize)
		if _, err := io.ReadFull(want, wantBuf[:n]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(file, gotBuf[:n]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, nil
			}
			return false, err
		}
		if !bytes.Equal(wantBuf[:n], gotBuf[:n]) {
			return false, nil
		}
		remaining -= n
	}
	return true, nil
}

// linkAnchor is a symlink entry that matched through the runner's copy of its target. The match
// holds only if the target is itself an archive entry the runner matched; otherwise the runner's
// target is content no archive entry vouches for, and a moved tag could ship anything there.
type linkAnchor struct {
	link, target string
}

// heldBy reports whether matched holds the anchor's target, or - for a directory - an entry below it.
func (a linkAnchor) heldBy(matched map[string]bool) bool {
	if matched[a.target] {
		return true
	}
	for rel := range matched {
		if strings.HasPrefix(rel, a.target+"/") {
			return true
		}
	}
	return false
}

// compareSymlinkEntry reports whether the runner holds the archive's symlink at rel, pointing at
// linkname, in any of the forms a runner leaves one in. A match that relies on the runner's copy
// of the target returns an anchor the caller must hold against the archive.
func compareSymlinkEntry(root, rel, linkname string) (same, missing bool, anchor *linkAnchor, err error) {
	linkPath := runnerPath(root, rel)
	info, err := os.Lstat(linkPath)
	if isMissing(err) {
		return false, true, nil, nil
	}
	if err != nil {
		return false, false, nil, err
	}
	targetRel, inside := linkTargetInside(rel, linkname)
	if inside {
		anchor = &linkAnchor{link: rel, target: targetRel}
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(linkPath)
		if err != nil {
			return false, false, nil, err
		}
		return target == linkname || target == filepath.FromSlash(linkname), false, anchor, nil
	case linksArriveAsText:
		// The file is the link text itself, so no target content is involved.
		if !info.Mode().IsRegular() || info.Size() != int64(len(linkname)) {
			return false, false, nil, nil
		}
		same, err = equalToFile(strings.NewReader(linkname), info.Size(), linkPath)
		return same, false, nil, err
	case !inside:
		return false, false, nil, nil
	case info.Mode().IsRegular():
		same, err = sameRunnerFile(runnerPath(root, targetRel), linkPath)
		return same, false, anchor, err
	case info.IsDir():
		same, err = sameRunnerTree(runnerPath(root, targetRel), linkPath)
		return same, false, anchor, err
	}
	return false, false, nil, nil
}

// linkTargetInside resolves the symlink at rel pointing at linkname to a path below the archive
// root, reporting false when the target is absolute or leaves the root.
func linkTargetInside(rel, linkname string) (string, bool) {
	if path.IsAbs(linkname) || filepath.IsAbs(linkname) {
		return "", false
	}
	target := path.Join(path.Dir(rel), linkname)
	if target == "." || !filepath.IsLocal(filepath.FromSlash(target)) {
		return "", false
	}
	return target, true
}

// sameRunnerFile reports whether got is a regular file with the bytes of the regular file want.
// A want that is missing or not a regular file is not an error: it just does not match.
func sameRunnerFile(want, got string) (same bool, err error) {
	wantInfo, err := os.Stat(want)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		return false, err
	}
	if !wantInfo.Mode().IsRegular() || !gotInfo.Mode().IsRegular() || wantInfo.Size() != gotInfo.Size() {
		return false, nil
	}
	file, err := os.Open(want)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return equalToFile(file, wantInfo.Size(), got)
}

// sameRunnerTree reports whether the directory got holds the same entries, with the same bytes or
// link targets, as the directory want.
func sameRunnerTree(want, got string) (bool, error) {
	wantInfo, err := os.Stat(want)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !wantInfo.IsDir() {
		return false, nil
	}
	wantEntries, err := listTree(want)
	if err != nil {
		return false, err
	}
	gotEntries, err := listTree(got)
	if err != nil {
		return false, err
	}
	if !slices.Equal(wantEntries, gotEntries) {
		return false, nil
	}
	for _, rel := range wantEntries {
		same, err := sameRunnerEntry(runnerPath(want, rel), runnerPath(got, rel))
		if err != nil || !same {
			return false, err
		}
	}
	return true, nil
}

// listTree returns the slash-separated paths of every non-directory entry below dir, in lexical
// order. Symlinks are listed, not followed.
func listTree(dir string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		entries = append(entries, filepath.ToSlash(rel))
		return nil
	})
	return entries, err
}

// sameRunnerEntry compares two entries of runner trees: symlinks by target, files by bytes.
func sameRunnerEntry(want, got string) (bool, error) {
	wantInfo, err := os.Lstat(want)
	if err != nil {
		return false, err
	}
	gotInfo, err := os.Lstat(got)
	if err != nil {
		return false, err
	}
	wantIsLink, gotIsLink := wantInfo.Mode()&os.ModeSymlink != 0, gotInfo.Mode()&os.ModeSymlink != 0
	if wantIsLink != gotIsLink {
		return false, nil
	}
	if wantIsLink {
		wantTarget, err := os.Readlink(want)
		if err != nil {
			return false, err
		}
		gotTarget, err := os.Readlink(got)
		return err == nil && wantTarget == gotTarget, err
	}
	return sameRunnerFile(want, got)
}
