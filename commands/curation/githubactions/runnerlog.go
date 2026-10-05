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

	// How the action was materialized. A symlink attempt that fails falls through to the archive and
	// then to a download, so a later, more expensive outcome wins over an earlier one.
	unpackedFoundRe = regexp.MustCompile(`Found unpacked action directory '[^']*[\\/]([0-9a-fA-F]{40,64})'`)
	archiveFoundRe  = regexp.MustCompile(`Found action archive '[^']*[\\/]([0-9a-fA-F]{40,64})\.(?:tar\.gz|zip)'`)
	requestURLRe    = regexp.MustCompile(`Request URL: https?://\S*/([0-9a-fA-F]{40,64}) `)
)

// LoggedAction is one action as the runner's "Set up job" output names it.
type LoggedAction struct {
	Owner, Repo, Ref, SHA string
}

// WorkerAction is one action the Worker log shows the runner materializing, with how it did.
type WorkerAction struct {
	Owner, Repo, SHA string
	Source           ActionSource
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
	source := map[string]ActionSource{}
	rank := map[ActionSource]int{SourceUnknown: 0, SourceCacheSymlink: 1, SourceCacheArchive: 2, SourceDownloaded: 3}
	note := func(sha string, s ActionSource) {
		sha = strings.ToLower(sha)
		if rank[s] > rank[source[sha]] {
			source[sha] = s
		}
	}
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
			if re == saveArchiveRe {
				// Written only on the download path, so it proves a download even when the
				// Request URL line is missing (that one needs an X-GitHub-Request-Id header).
				note(sha, SourceDownloaded)
			}
		}
		if m := unpackedFoundRe.FindStringSubmatch(line); m != nil {
			note(m[1], SourceCacheSymlink)
		}
		if m := archiveFoundRe.FindStringSubmatch(line); m != nil {
			note(m[1], SourceCacheArchive)
		}
		if m := requestURLRe.FindStringSubmatch(line); m != nil {
			note(m[1], SourceDownloaded)
		}
	}
	for i := range actions {
		actions[i].Source = source[actions[i].SHA]
	}
	return actions
}
