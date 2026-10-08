package githubactions

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CacheEntryReadOnly reports whether the runner's action-archive cache entry for a cannot be changed
// by the user this process runs as - the same user as the job's steps - because that user neither
// owns nor can write it. Only then may an action the Worker loaded from that cache be decided by its
// logged SHA: a job that can change the cache can plant any content under an approved SHA for the
// jobs after it.
//
// cacheDir is the cache root the runner logged (ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE). The entry is
// located by the runner's own layout - <cacheDir>/<owner>_<repo>/<sha>.tar.gz (or .zip), or the
// unpacked <sha> directory the symlink cache links into _actions - never by a path from the log.
// Every such entry present must be locked (see pathLocked), together with each directory from it up
// to cacheDir, cacheDir included; for an unpacked directory that is every path in its tree, since
// the job writes through the symlink into it. A symlink inside that tree is never trusted, as its
// target lies outside what is checked. An entry that cannot be located or examined is not trusted,
// and on Windows, where the check is not made, no entry is ever trusted.
func CacheEntryReadOnly(cacheDir string, a WorkerAction) bool {
	if cacheDir == "" || !filepath.IsAbs(cacheDir) || a.SHA == "" {
		return false
	}
	entries := cacheEntries(cacheDir, a)
	if len(entries) == 0 || !pathLocked(cacheDir) {
		return false
	}
	for _, entry := range entries {
		if !pathLocked(filepath.Dir(entry)) || !treeLocked(entry) {
			return false
		}
	}
	return true
}

// cacheEntries returns every path under cacheDir the runner could have loaded a from. Names are
// matched case-insensitively: the runner names the folder after the repository as GitHub resolved
// it, whose case the Worker log does not reliably keep. A folder that is a symlink is not followed,
// since its target lies outside what is checked; a.SHA is lower-case hex, so it cannot leave the folder.
func cacheEntries(cacheDir string, a WorkerAction) []string {
	folders, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil
	}
	var entries []string
	for _, folder := range folders {
		if !strings.EqualFold(folder.Name(), a.Owner+"_"+a.Repo) {
			continue
		}
		if !folder.IsDir() {
			return nil
		}
		files, err := os.ReadDir(filepath.Join(cacheDir, folder.Name()))
		if err != nil {
			return nil
		}
		for _, f := range files {
			for _, name := range []string{a.SHA + ".tar.gz", a.SHA + ".zip", a.SHA} {
				if strings.EqualFold(f.Name(), name) {
					entries = append(entries, filepath.Join(cacheDir, folder.Name(), f.Name()))
				}
			}
		}
	}
	return entries
}

// pathLocked reports whether the current user can neither write nor chmod one path. It is a variable
// so that tests, which cannot create paths another user owns without root, can stand in for it.
var pathLocked = osPathLocked

// treeLocked reports whether path, and for a directory every path below it, is locked and not a
// symlink.
func treeLocked(path string) bool {
	locked := true
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || !pathLocked(p) {
			locked = false
			return filepath.SkipAll
		}
		return nil
	})
	return err == nil && locked
}
