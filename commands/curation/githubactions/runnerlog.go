package githubactions

import (
	"regexp"
	"strings"
)

// The line formats below are the runner's own (actions/runner, src/Runner.Worker/ActionManager.cs,
// v2.337.0). They are undocumented, so every caller must treat an empty result as "the runner did
// not say" and fall back to walking the action cache.
var (
	// The "Set up job" step prints this per action (:1212). It is the only line that names the ref
	// and the SHA together.
	setupJobLineRe = regexp.MustCompile(`Download action repository '([^/'@]+)/([^'@]+)@([^']+)' \(SHA:([0-9a-fA-F]{40}|[0-9a-fA-F]{64})\)`)

	// Which of these three names an action depends on the cache configuration: the symlink check
	// only runs with ACTIONS_RUNNER_SYMLINK_CACHED_ACTIONS (:1239), the archive check only with
	// ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE (:1281), and a runner with neither logs only the download.
	symlinkCheckRe = regexp.MustCompile(`Checking if can symlink '([^/'@]+)/([^'@]+)@([0-9a-fA-F]{40,64})'`)
	archiveCheckRe = regexp.MustCompile(`Check if action archive '([^/'@]+)/([^'@]+)@([0-9a-fA-F]{40,64})'`)
	saveArchiveRe  = regexp.MustCompile(`Save archive 'https?://[^']*?/([^/']+)/([^/']+)/(?:tar\.gz|zip|tarball|zipball)/([0-9a-fA-F]{40,64})'`)
)

// LoggedAction is one action as the runner's "Set up job" output names it.
type LoggedAction struct {
	Owner, Repo, Ref, SHA string
}

// WorkerAction is one action the Worker log shows the runner materializing.
type WorkerAction struct {
	Owner, Repo, SHA string
}

// ParseSetupJobLines returns each action the "Set up job" output names, once, in the order named.
func ParseSetupJobLines(text string) []LoggedAction {
	var actions []LoggedAction
	seen := map[string]bool{}
	for _, m := range setupJobLineRe.FindAllStringSubmatch(text, -1) {
		action := LoggedAction{Owner: m[1], Repo: m[2], Ref: m[3], SHA: strings.ToLower(m[4])}
		key := strings.ToLower(action.Owner + "/" + action.Repo + "@" + action.Ref)
		if seen[key] {
			continue
		}
		seen[key] = true
		actions = append(actions, action)
	}
	return actions
}

// ParseWorkerLog returns each action the Worker log shows the runner materializing, once, in the
// order the runner first logged it, with its SHA lower-cased.
func ParseWorkerLog(text string) []WorkerAction {
	var actions []WorkerAction
	index := map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		for _, re := range []*regexp.Regexp{symlinkCheckRe, archiveCheckRe, saveArchiveRe} {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			sha := strings.ToLower(m[3])
			key := strings.ToLower(m[1]+"/"+m[2]) + "@" + sha
			if _, ok := index[key]; !ok {
				index[key] = len(actions)
				actions = append(actions, WorkerAction{Owner: strings.ToLower(m[1]), Repo: strings.ToLower(m[2]), SHA: sha})
			}
		}
	}
	return actions
}
