package githubactions

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// CallerMode is where in the job the command runs, which decides what the log's position proves.
type CallerMode int

const (
	// The zero value is no known mode, which is never trusted.

	// ModeHook is the self-hosted job-started hook, which runs before any step of the job.
	ModeHook CallerMode = iota + 1
	// ModePre is the pre script of an action (--from-pre), trusted only when that action is step 1
	// and nothing else started before it.
	ModePre
)

// The fixed reason codes shown in an action's Notes when it is verified by content although the
// runner's log named a commit. They are codes rather than prose so no log text reaches the report.
const (
	ReasonNoSHA        = "no-sha"
	ReasonLogUntrusted = "log-untrusted"
	ReasonStaleLine    = "stale-line"
	// ReasonRenamed is assigned when a ref is paired with a SHA logged under another repository
	// name; names alone cannot tell a rename from a remote composite's inner action, so AssessLogs
	// never sets it.
	ReasonRenamed     = "renamed"
	ReasonCacheSource = "cache-source"
)

// LogEvidence is everything the trust assessment looks at.
type LogEvidence struct {
	Snapshot DiagSnapshot
	Mode     CallerMode
	Self     string // "owner/repo" of the running action; only meaningful for ModePre
	// GitHubHosted is false in hook mode (a hook runs only on a self-hosted runner); in pre mode it is
	// RUNNER_ENVIRONMENT == "github-hosted".
	GitHubHosted bool
	// Refs are the actions found on disk or in the setup lines. "An action of this job" for R3 means the
	// job-message steps plus these refs.
	Refs []ActionRef
	// TrustedCache says the cache entry the action was loaded from cannot be changed by this job's user,
	// so R7 does not apply to it. nil means never trusted.
	TrustedCache func(WorkerAction) bool
}

// LogAssessment says which of the Worker log's SHAs may be approved without reading the runner's copy.
type LogAssessment struct {
	// JobTrusted is false when a provenance rule (R1, R2, R5, R6) failed: every action then goes to
	// content comparison.
	JobTrusted bool
	// JobReasons holds one sentence per failed provenance rule, for the one job-level warning. Values
	// from outside appear only as owner/repo, quoted and without control characters.
	JobReasons []string
	// ActionReason maps AssessmentKey to a reason code for each action an evidence rule (R3, R4, R7)
	// downgrades. An action two rules hit keeps the first one set, in the order stale-line,
	// cache-source, so the code shown does not depend on map order.
	ActionReason map[string]string
}

// AssessmentKey is the ActionReason map key: lower-case "owner/repo@sha". A repository step with no
// Worker entry has no SHA, so its key is "owner/repo@".
func AssessmentKey(owner, repo, sha string) string {
	return repoKey(owner, repo) + "@" + strings.ToLower(sha)
}

// AssessLogs decides whether the runner's logs may stand in for reading the actions' copies. Provenance
// rules judge where the logs came from and so the whole job; evidence rules judge one action's entries.
// A failed rule is a downgrade to content comparison, never an error.
func AssessLogs(e LogEvidence) LogAssessment {
	reasons := provenanceReasons(e)
	return LogAssessment{
		JobTrusted:   len(reasons) == 0,
		JobReasons:   reasons,
		ActionReason: evidenceReasons(e),
	}
}

// provenanceReasons returns one sentence per failed provenance rule. Only Self is taken from outside,
// so it is the only value that has to be sanitised.
func provenanceReasons(e LogEvidence) []string {
	var reasons []string
	job := e.Snapshot.Job
	switch e.Mode {
	case ModeHook:
		// Runs before any step of this job; what it reads still has to pass the rules below. A job
		// message without steps cannot vouch for the job either.
		if !e.Snapshot.JobParsed || len(job.Steps) == 0 {
			reasons = append(reasons, "the job message could not be read")
		}
	case ModePre:
		switch {
		case e.Self == "" || len(job.Steps) == 0:
			reasons = append(reasons, "this action's name or the job message's steps are unknown, so its position in the job cannot be proven")
		case !isRemoteStep(job.Steps[0], e.Self):
			reasons = append(reasons, fmt.Sprintf("%q is not the first step of the job", sanitizeForLog(e.Self)))
		case countRemoteSteps(job.Steps, e.Self) > 1:
			// The pre of a later use of the same action also finds itself named first, after the
			// steps before it have run their own pre.
			reasons = append(reasons, fmt.Sprintf("%q appears more than once in the job, so this pre's position cannot be proven",
				sanitizeForLog(e.Self)))
		case job.HasServices || job.HasContainer || e.Snapshot.BuildsImage:
			// A docker:// pull does not count: it runs no code.
			reasons = append(reasons, "the job starts a service, a container or an image build before the first step's pre")
		}
	default:
		reasons = append(reasons, "the caller's position in the job is unknown")
	}
	if e.Snapshot.Planted {
		reasons = append(reasons, "a Worker log other than the job's own is present")
	}
	if e.Snapshot.SetupBufferUnfiltered {
		reasons = append(reasons, "the setup buffer cannot be told from other jobs'")
	}
	return reasons
}

// isRemoteStep reports whether s runs the GitHub repository action named self (owner/repo).
func isRemoteStep(s JobStep, self string) bool {
	return strings.EqualFold(s.Type, "repository") && strings.EqualFold(s.RepositoryType, "github") &&
		strings.EqualFold(s.Name, self)
}

// countRemoteSteps returns how many of steps run the GitHub repository action named self.
func countRemoteSteps(steps []JobStep, self string) int {
	n := 0
	for _, s := range steps {
		if isRemoteStep(s, self) {
			n++
		}
	}
	return n
}

// evidenceReasons applies the per-action rules: R3 a setup line for one of this job's actions naming
// a SHA the Worker never fetched (which downgrades that repository's logged commits), R4 a repository
// step with no Worker entry, and R7 an action loaded from a cache this job's user may have written on
// a self-hosted runner.
func evidenceReasons(e LogEvidence) map[string]string {
	reasons := map[string]string{}
	set := func(key, code string) {
		if _, ok := reasons[key]; !ok {
			reasons[key] = code
		}
	}

	fetched := map[string]bool{}
	logged := map[string]bool{}
	for _, w := range e.Snapshot.Worker {
		fetched[AssessmentKey(w.Owner, w.Repo, w.SHA)] = true
		logged[repoKey(w.Owner, w.Repo)] = true
	}

	ofJob := map[string]bool{}
	for _, s := range e.Snapshot.Job.Steps {
		if s.Name != "" {
			ofJob[strings.ToLower(s.Name)] = true
		}
	}
	for _, r := range e.Refs {
		ofJob[repoKey(r.Owner, r.Repo)] = true
	}

	// R3: the ref need not match; a line for a repository outside this job may be another job's. The
	// stale line's own SHA is never paired with a ref (the Worker did not log it), so the downgrade goes
	// to every commit the Worker logged for that repository and to its SHA-less key.
	// A line with an empty SHA matches no Worker entry and so counts as stale, which fails safe.
	for _, l := range e.Snapshot.SetupJob {
		repo := repoKey(l.Owner, l.Repo)
		if !ofJob[repo] || fetched[AssessmentKey(l.Owner, l.Repo, l.SHA)] {
			continue
		}
		for _, w := range e.Snapshot.Worker {
			if repoKey(w.Owner, w.Repo) == repo {
				set(AssessmentKey(w.Owner, w.Repo, w.SHA), ReasonStaleLine)
			}
		}
		set(AssessmentKey(l.Owner, l.Repo, ""), ReasonStaleLine)
	}

	// R7: a GitHub-hosted runner starts from a fresh image, so only a self-hosted cache can be written
	// by an earlier job.
	if !e.GitHubHosted {
		for _, w := range e.Snapshot.Worker {
			if w.Source == SourceCache && (e.TrustedCache == nil || !e.TrustedCache(w)) {
				set(AssessmentKey(w.Owner, w.Repo, w.SHA), ReasonCacheSource)
			}
		}
	}

	// R4 exempts by type: local, docker:// and script steps never get a Worker entry.
	for _, s := range e.Snapshot.Job.Steps {
		if !strings.EqualFold(s.RepositoryType, "github") {
			continue
		}
		owner, repo, ok := strings.Cut(s.Name, "/")
		if !ok || logged[repoKey(owner, repo)] {
			continue
		}
		set(AssessmentKey(owner, repo, ""), ReasonNoSHA)
	}
	return reasons
}

// sanitizeForLog drops control characters from s, so a value taken from the job or the environment
// cannot end a line and start a workflow command (::error:: ...) or a terminal escape on our stdout.
func sanitizeForLog(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// QuoteForLog quotes s for a log line with its control characters dropped, so a value read from the
// runner's logs or folders cannot start a workflow command (::error:: ...) on our stdout.
func QuoteForLog(s string) string {
	return strconv.Quote(sanitizeForLog(s))
}
