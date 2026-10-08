package githubactions

import (
	"path/filepath"
	"regexp"
	"strings"
)

// The line formats below are the runner's own (actions/runner, src/Runner.Worker/ActionManager.cs,
// v2.337.0). They are undocumented, so every caller must treat an empty result as "the runner did
// not say" and fall back to walking the action cache.
var (
	// The "Set up job" step prints this per action (:1212). It is the only line that names the ref
	// and the SHA together. It is matched anywhere on a line, so no capture may cross a line break.
	setupJobLineRe = regexp.MustCompile(`Download action repository '([^/'@\r\n]+)/([^'@\r\n]+)@([^'\r\n]+)' \(SHA:([0-9a-fA-F]{40}|[0-9a-fA-F]{64})\)`)

	// Which of these three names an action depends on the cache configuration: the symlink check
	// only runs with ACTIONS_RUNNER_SYMLINK_CACHED_ACTIONS (:1239), the archive check only with
	// ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE (:1281), and a runner with neither logs only the download.
	symlinkCheckRe = regexp.MustCompile(workerLinePrefix + `Checking if can symlink '([^/'@]+)/([^'@]+)@([0-9a-fA-F]{40,64})'`)
	archiveCheckRe = regexp.MustCompile(workerLinePrefix + `Check if action archive '([^/'@]+)/([^'@]+)@([0-9a-fA-F]{40,64})'(.*)`)
	saveArchiveRe  = regexp.MustCompile(workerLinePrefix + `Save archive 'https?://[^']*?/([^/']+)/([^/']+)/(?:tar\.gz|zip|tarball|zipball)/([0-9a-fA-F]{40,64})'`)

	// foundUnpackedRe follows a symlink check whose unpacked directory exists (:1245). It is the only
	// line that names the cache directory of an action the symlink cache serves.
	foundUnpackedRe = regexp.MustCompile(workerLinePrefix + `Found unpacked action directory '([^']*)' in cache directory '([^']*)'`)
	// foundArchiveRe follows an archive check whose archive exists (:1291). It names no action the
	// other lines do not, but its cache directory must agree with theirs.
	foundArchiveRe = regexp.MustCompile(workerLinePrefix + `Found action archive '[^']*' in cache directory '([^']*)'`)
)

// workerLinePrefix is how the runner starts every log line. Anchoring on it keeps text that only
// quotes a runner line, such as a job message the workflow author controls, from being read as one.
const workerLinePrefix = `^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}Z [A-Z ]+ ActionManager\] `

var (
	// cacheDirRe reads the directory off the tail of an archive-check line. The runner quotes it
	// ("in cache directory '<dir>'"); the unquoted "in cache dir <dir>" form is accepted too.
	cacheDirRe = regexp.MustCompile(`in cache dir(?:ectory)? (?:'([^']*)'|(\S.*))`)

	// imageBuildRes match the two ActionManager lines that mean an action is built from a Dockerfile.
	// A pull ("needs to pull image") runs no code before pre, so it is not matched.
	imageBuildRes = []*regexp.Regexp{
		regexp.MustCompile(workerLinePrefix + `\d+ steps? need to build image from '`),
		regexp.MustCompile(workerLinePrefix + `Action .* from repository '[^']*' needs to build image '`),
	}
)

// LoggedAction is one action as the runner's "Set up job" output names it.
type LoggedAction struct {
	Owner, Repo, Ref, SHA string
}

// WorkerAction is one action the Worker log shows the runner materializing.
type WorkerAction struct {
	Owner, Repo, SHA string
	// Source is SourceSave when the runner downloaded the action and SourceCache when it only
	// checked its caches.
	Source string
	// CacheDir is the archive cache directory the runner reported for this action, from its archive
	// check or its unpacked directory. It is empty when the runner reported none, or when the log names
	// more than one cache directory: the runner has one, so the others are forged, and which is
	// unknown.
	CacheDir string
}

// The values of WorkerAction.Source.
const (
	SourceSave  = "save"
	SourceCache = "cache"
)

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
// order the runner first logged it, with its owner, repo and SHA lower-cased. Only runner
// ActionManager lines count. A Save archive line for an action makes its Source SourceSave
// wherever it appears relative to the check lines.
func ParseWorkerLog(text string) []WorkerAction {
	var actions []WorkerAction
	index := map[string]int{}
	roots := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if m := foundArchiveRe.FindStringSubmatch(line); m != nil {
			roots[filepath.Clean(m[1])] = true
			continue
		}
		if m := foundUnpackedRe.FindStringSubmatch(line); m != nil {
			roots[filepath.Clean(m[2])] = true
			if i, ok := index[unpackedEntryKey(m[1])]; ok && actions[i].CacheDir == "" {
				actions[i].CacheDir = m[2]
			}
			continue
		}
		for _, re := range []*regexp.Regexp{symlinkCheckRe, archiveCheckRe, saveArchiveRe} {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			sha := strings.ToLower(m[3])
			key := strings.ToLower(m[1]+"/"+m[2]) + "@" + sha
			i, ok := index[key]
			if !ok {
				i = len(actions)
				index[key] = i
				actions = append(actions, WorkerAction{Owner: strings.ToLower(m[1]), Repo: strings.ToLower(m[2]), SHA: sha, Source: SourceCache})
			}
			switch re {
			case saveArchiveRe:
				actions[i].Source = SourceSave
			case archiveCheckRe:
				dir := parseCacheDir(m[4])
				if dir != "" {
					roots[filepath.Clean(dir)] = true
				}
				if actions[i].CacheDir == "" {
					actions[i].CacheDir = dir
				}
			}
		}
	}
	if len(roots) > 1 {
		for i := range actions {
			actions[i].CacheDir = ""
		}
	}
	return actions
}

// unpackedEntryKey returns the index key of the action whose unpacked directory is entry, the
// runner's <cache>/<owner>_<repo>/<sha>. GitHub owner names hold no underscore, so the first one in
// the folder name separates owner from repo.
func unpackedEntryKey(entry string) string {
	parts := strings.FieldsFunc(entry, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) < 2 {
		return ""
	}
	owner, repo, ok := strings.Cut(parts[len(parts)-2], "_")
	if !ok {
		return ""
	}
	return strings.ToLower(owner+"/"+repo) + "@" + strings.ToLower(parts[len(parts)-1])
}

func parseCacheDir(tail string) string {
	m := cacheDirRe.FindStringSubmatch(tail)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return strings.TrimSpace(m[2])
}

// WorkerLogBuildsImage reports whether the Worker log shows the runner building an image from a
// Dockerfile, which runs author code before any pre step.
func WorkerLogBuildsImage(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, re := range imageBuildRes {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}
