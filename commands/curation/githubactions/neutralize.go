package githubactions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Neutralize removes the runner's copy of every action in refs from actionsRoot, the _actions
// directory the refs were discovered under, and returns what it removed as "owner/repo@ref".
//
// The runner reads an action's action.yml from _actions each time one of its steps starts, pre and
// post included, so an action whose folder is gone fails at load before any of its code runs. Exiting
// non-zero alone does not do that: other actions' pre and post still run after a failed pre, and
// continue-on-error on the failing step hides the failure altogether.
//
// A ref decided by the content of its own folder removes that folder and its watermark. A ref decided
// by a commit - an unpaired logged commit, a ref paired with a logged SHA - removes every ref folder of
// its repository (never the owner or repository folder itself), since any of them may hold that
// commit: removing too much costs a failed step, removing too little leaves the rejected commit
// runnable.
//
// A folder that is a symlink into the runner's shared cache is unlinked, never emptied through the
// link. A path whose parent does not resolve inside actionsRoot is refused. A failure is collected
// with the path it concerns and the remaining refs are still removed.
func Neutralize(actionsRoot string, refs []ActionRef) (removed []string, err error) {
	root, err := filepath.EvalSymlinks(actionsRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving the runner's action directory %q: %w", actionsRoot, err)
	}
	var errs []error
	wholeRepos := map[string]bool{}
	for _, ref := range refs {
		if ref.Path != "" {
			gone, removeErr := removeFolder(root, ref.Path)
			errs = append(errs, removeErr)
			if gone {
				removed = append(removed, ref.Owner+"/"+ref.Repo+"@"+ref.Ref)
			}
			// A folder that was not there (the runner spelled it otherwise) falls back to the repository;
			// a refused or failed one does not, so a path outside the root never widens the removal.
			if decidedByFolder(ref) && (gone || removeErr != nil) {
				continue
			}
		}
		key := strings.ToLower(ref.Owner + "/" + ref.Repo)
		if wholeRepos[key] {
			continue
		}
		wholeRepos[key] = true
		names, removeErr := removeRepository(root, ref.Owner, ref.Repo)
		removed = append(removed, names...)
		errs = append(errs, removeErr)
	}
	return removed, errors.Join(errs...)
}

// NeutralizeAll removes every action folder and watermark under actionsRoot/<owner>/<repo>, for a run
// that reached no verdict at all: nothing was approved, so nothing may run. The owner and repository
// folders themselves, and the root, stay. It returns what it removed as "owner/repo@ref".
func NeutralizeAll(actionsRoot string) (removed []string, err error) {
	root, err := filepath.EvalSymlinks(actionsRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving the runner's action directory %q: %w", actionsRoot, err)
	}
	var errs []error
	for _, ownerDir := range childDirs(root, &errs) {
		for _, repoDir := range childDirs(ownerDir, &errs) {
			names, removeErr := removeRefFolders(root, repoDir)
			removed = append(removed, names...)
			errs = append(errs, removeErr)
		}
	}
	return removed, errors.Join(errs...)
}

// childDirs returns every entry of dir that resolves to a directory, recording a listing that fails.
// An entry that links elsewhere is returned too: removeInside refuses what lies outside the root.
func childDirs(dir string, errs *[]error) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		*errs = append(*errs, pathError("listing", dir, err))
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			dirs = append(dirs, path)
		}
	}
	return dirs
}

// decidedByFolder reports whether ref's verdict is about its own folder's content rather than about a
// commit that another folder of the repository could hold as well.
func decidedByFolder(ref ActionRef) bool {
	return ref.Path != "" && ref.Verification == VerifyContent && len(ref.LoggedSHAs) == 0
}

// removeFolder removes one action folder and its watermark, reporting whether the folder was there.
func removeFolder(root, path string) (bool, error) {
	gone, err := removeInside(root, path)
	_, watermarkErr := removeInside(root, path+watermarkSuffix)
	return gone, errors.Join(err, watermarkErr)
}

// removeRepository removes every ref folder and watermark under root/<owner>/<repo>, matching the
// names regardless of case, since the runner names folders verbatim from uses: and a Worker-logged
// commit carries GitHub's spelling. The repository folder itself stays. It returns the ref folders it
// removed as "owner/repo@ref".
func removeRepository(root, owner, repo string) ([]string, error) {
	var removed []string
	var errs []error
	for _, ownerDir := range matchingDirs(root, owner, &errs) {
		for _, repoDir := range matchingDirs(ownerDir, repo, &errs) {
			names, err := removeRefFolders(root, repoDir)
			removed = append(removed, names...)
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

// removeRefFolders removes every entry of repoDir - ref folders, the first segment of a slashed ref,
// watermarks - and never repoDir itself. A removed folder is named by the refs the cache walk finds
// in it, so a slashed branch reads as owner/repo@copilot/backport rather than owner/repo@copilot.
func removeRefFolders(root, repoDir string) ([]string, error) {
	entries, err := os.ReadDir(repoDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, pathError("listing", repoDir, err)
	}
	name := filepath.Base(filepath.Dir(repoDir)) + "/" + filepath.Base(repoDir)
	refsUnder := map[string][]string{}
	if roots, _, scanErr := scanRepo(repoDir); scanErr == nil {
		for _, r := range roots {
			first, _, _ := strings.Cut(r.ref, "/")
			refsUnder[first] = append(refsUnder[first], name+"@"+r.ref)
		}
	}
	var removed []string
	var errs []error
	for _, entry := range entries {
		gone, removeErr := removeInside(root, filepath.Join(repoDir, entry.Name()))
		errs = append(errs, removeErr)
		if !gone || isWatermarkFile(entry) {
			continue
		}
		if refs := refsUnder[entry.Name()]; len(refs) > 0 {
			removed = append(removed, refs...)
		} else {
			removed = append(removed, name+"@"+entry.Name())
		}
	}
	return removed, errors.Join(errs...)
}

// matchingDirs returns the entries of dir named name regardless of case. A listing that fails is
// recorded in errs, except for a dir that does not exist.
func matchingDirs(dir, name string, errs *[]error) []string {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		*errs = append(*errs, pathError("listing", dir, err))
		return nil
	}
	var matches []string
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), name) {
			matches = append(matches, filepath.Join(dir, entry.Name()))
		}
	}
	return matches
}

// removeInside removes path, which must lie below root, and reports whether it was there. The parent
// is resolved and the leaf is not: a leaf that is a symlink is removed itself, never followed, while a
// parent that resolves outside root (a symlinked owner folder, a "..") is refused. root is resolved.
func removeInside(root, path string) (bool, error) {
	leaf := filepath.Base(path)
	if !filepath.IsAbs(path) || leaf == "." || leaf == ".." || leaf == string(filepath.Separator) {
		return false, refusal(path, root)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, pathError("resolving", path, err)
	}
	// "." is root itself: an owner folder is never removed, only what lies below it.
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false, refusal(path, root)
	}
	target := filepath.Join(parent, leaf)
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, pathError("removing", path, err)
	}
	// A symlink, or a Windows junction, is not a directory to Lstat: os.Remove unlinks it and leaves the
	// shared cache it points into. RemoveAll does not follow links below target either.
	if info.IsDir() {
		err = os.RemoveAll(target)
	} else {
		err = os.Remove(target)
	}
	if err != nil {
		return false, pathError("removing", path, err)
	}
	return true, nil
}

// pathError names path and quotes the file an *fs.PathError names: RemoveAll reports a file inside the
// action, whose name the action's author chose and could use to start a workflow command on our output.
func pathError(op, path string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s %q: %s %s: %w", op, path, pe.Op, QuoteForLog(pe.Path), pe.Err)
	}
	return fmt.Errorf("%s %q: %s", op, path, QuoteForLog(err.Error()))
}

func refusal(path, root string) error {
	return fmt.Errorf("refusing to remove %q: it is not inside the runner's action directory %q", path, root)
}
