package githubactions

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// AttachRunnerProvenance gives each walked action the SHA the runner's logs name for it.
// refs keeps walked's order and length; unplaced is every Worker action no ref was given.
//
// The ref for a SHA comes from, in order:
//  1. the "Set up job" line, which names ref and SHA together;
//  2. the ref folder's name, when it is the SHA itself;
//  3. a symlink into the unpacked cache, whose target path holds the SHA;
//  4. the only ref of that action left, when one SHA is left too;
//  5. download order: the runner materializes actions one at a time and writes <ref>.completed
//     after each, so the k-th SHA in log order belongs to the k-th ref by watermark time.
//
// A ref is never guessed: tied watermark times, or SHAs and refs that do not pair one to one, leave
// the runner SHA empty. Those refs are still curated, by content.
func AttachRunnerProvenance(walked []ActionRef, setupJob []LoggedAction, worker []WorkerAction) ([]ActionRef, []WorkerAction) {
	refs := slices.Clone(walked)
	placed := make([]bool, len(worker))
	isPlaced := func(w WorkerAction) bool {
		for k := range worker {
			if placed[k] && worker[k] == w {
				return true
			}
		}
		return false
	}
	// bySHA returns the unplaced Worker entry for sha, preferring one named repo. Another name is
	// accepted because the Worker log uses the name GitHub resolved the action to, which differs from
	// the one the workflow wrote (and the _actions folder carries) after a repository transfer.
	bySHA := func(repo, sha string) int {
		found := -1
		for k, w := range worker {
			if placed[k] || !strings.EqualFold(w.SHA, sha) {
				continue
			}
			if repoKey(w.Owner, w.Repo) == repo {
				return k
			}
			if found < 0 {
				found = k
			}
		}
		return found
	}
	assign := func(i int, sha string) {
		if k := bySHA(repoKey(refs[i].Owner, refs[i].Repo), sha); k >= 0 {
			placed[k] = true
			refs[i].RunnerSHA = worker[k].SHA
		}
	}

	// A setup line counts only when this job's Worker log fetched its SHA: a buffer left over from a
	// job whose upload failed, or one a job planted, must not decide which commit this job ran.
	for i, r := range refs {
		for _, l := range setupJob {
			if repoKey(l.Owner, l.Repo) == repoKey(r.Owner, r.Repo) && l.Ref == r.Ref {
				assign(i, l.SHA)
				break
			}
		}
	}
	for i, r := range refs {
		if r.RunnerSHA == "" {
			assign(i, r.Ref)
		}
	}

	for _, key := range repoKeysInOrder(worker) {
		var open []int
		for i, r := range refs {
			if r.RunnerSHA == "" && repoKey(r.Owner, r.Repo) == key {
				open = append(open, i)
			}
		}
		var shas []string
		for _, w := range worker {
			if !isPlaced(w) && repoKey(w.Owner, w.Repo) == key {
				shas = append(shas, w.SHA)
			}
		}
		// Symlinks name their SHA in the target path.
		var rest []int
		for _, i := range open {
			if sha := linkedSHA(refs[i].Path, shas); sha != "" {
				assign(i, sha)
				shas = slices.DeleteFunc(shas, func(s string) bool { return s == sha })
				continue
			}
			rest = append(rest, i)
		}
		switch {
		case len(rest) == 1 && len(shas) == 1:
			assign(rest[0], shas[0])
		case len(rest) > 1 && len(rest) == len(shas):
			if ordered, ok := byWatermarkTime(refs, rest); ok {
				for k, i := range ordered {
					assign(i, shas[k])
				}
			}
		}
	}

	var unplaced []WorkerAction
	for _, w := range worker {
		if !isPlaced(w) {
			unplaced = append(unplaced, w)
		}
	}
	return refs, unplaced
}

func repoKey(owner, repo string) string { return strings.ToLower(owner + "/" + repo) }

func repoKeysInOrder(worker []WorkerAction) []string {
	var keys []string
	for _, w := range worker {
		if key := repoKey(w.Owner, w.Repo); !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// linkedSHA returns the SHA among shas that the symlink at path points into, or "" when path is not
// a symlink or names none of them. The runner links to <cache>/<owner>_<repo>/<sha>/<one directory>.
func linkedSHA(path string, shas []string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	segments := strings.Split(filepath.ToSlash(target), "/")
	for _, sha := range shas {
		for _, segment := range segments {
			if strings.EqualFold(segment, sha) {
				return sha
			}
		}
	}
	return ""
}

// byWatermarkTime orders the ref indexes by when the runner finished extracting each, reporting
// false when a watermark is missing or two share a time, since the order would then be a guess.
func byWatermarkTime(refs []ActionRef, indexes []int) ([]int, bool) {
	times := map[int]time.Time{}
	for _, i := range indexes {
		info, err := os.Stat(refs[i].Path + watermarkSuffix)
		if err != nil {
			return nil, false
		}
		times[i] = info.ModTime()
	}
	ordered := slices.Clone(indexes)
	slices.SortFunc(ordered, func(a, b int) int { return times[a].Compare(times[b]) })
	for k := 1; k < len(ordered); k++ {
		if times[ordered[k]].Equal(times[ordered[k-1]]) {
			return nil, false
		}
	}
	return ordered, true
}

// RefsFromSetupJob builds the actions straight from the "Set up job" lines, without walking the
// action cache, when those lines account for this job exactly: each Worker action has a line with
// its SHA, and each line names a SHA this job's Worker log fetched. It reports false otherwise - the
// buffer is gone or partly deleted, or a line is stale - and the caller walks the cache instead.
//
// SHAs, not names, are matched: after a repository transfer the Worker log uses the name GitHub
// resolved, while the line and the _actions folder keep the one the workflow wrote.
func RefsFromSetupJob(setupJob []LoggedAction, worker []WorkerAction, actionsCacheDir string) ([]ActionRef, bool) {
	if len(setupJob) == 0 {
		return nil, false
	}
	named := map[string]bool{}
	for _, l := range setupJob {
		named[strings.ToLower(l.SHA)] = true
	}
	fetched := map[string]bool{}
	for _, w := range worker {
		if !named[w.SHA] {
			return nil, false
		}
		fetched[w.SHA] = true
	}
	refs := make([]ActionRef, 0, len(setupJob))
	for _, l := range setupJob {
		sha := strings.ToLower(l.SHA)
		if !fetched[sha] {
			return nil, false
		}
		refs = append(refs, ActionRef{
			Owner:     l.Owner,
			Repo:      l.Repo,
			Ref:       l.Ref,
			Path:      filepath.Join(actionsCacheDir, l.Owner, l.Repo, filepath.FromSlash(l.Ref)),
			RunnerSHA: sha,
		})
	}
	return refs, true
}
