package curation

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"

	"github.com/jfrog/jfrog-cli-security/commands/curation/githubactions"
)

// actionDiscoverer lists the actions this job resolved, in the order the report shows them. A step
// on a GitHub-hosted runner can only walk the action cache; the job-started hook on a self-hosted
// runner can also read the runner's own logs, which name the SHA of each action.
type actionDiscoverer interface {
	Discover() ([]githubactions.ActionRef, error)
}

// cacheWalkDiscoverer lists the actions by walking the runner's _actions directory.
type cacheWalkDiscoverer struct {
	actionsCacheDir string
}

func (d cacheWalkDiscoverer) Discover() ([]githubactions.ActionRef, error) {
	scan, err := githubactions.DiscoverActionCache(d.actionsCacheDir)
	if err != nil {
		return nil, err
	}
	if err = scan.UnaccountedError(); err != nil {
		return nil, err
	}
	if len(scan.Refs) == 0 {
		return nil, githubactions.ErrCacheNotReadable()
	}
	return scan.Refs, nil
}

// runnerLogDiscoverer lists the actions from what only the runner's own logs know: the SHA it fetched
// for each action. It runs as the self-hosted job-started hook, or in an action's pre script. It walks
// the action cache only when the "Set up job" lines cannot account for every action, or when the job's
// logs cannot be trusted, to recover the refs from the folders themselves.
//
// The logs decide an action by its SHA only when the trust assessment says they may (markVerification);
// otherwise the runner's copy is compared with the content at that SHA.
type runnerLogDiscoverer struct {
	runnerDir       string
	actionsCacheDir string
	// mode is where the command runs; the zero value is unknown, and the logs are then never trusted.
	mode githubactions.CallerMode
	// self is the "owner/repo" of the action running the command in githubactions.ModePre.
	self string
	// githubHosted is true on a GitHub-hosted runner, whose action cache no earlier job can have written.
	githubHosted bool
	// trustActionCache is the admin's --trust-action-cache: every action loaded from the runner's
	// archive or symlink cache is trusted as if that cache were read-only to the job.
	trustActionCache bool
}

func (d runnerLogDiscoverer) Discover() ([]githubactions.ActionRef, error) {
	// First, before anything slow: the runner deletes the "Set up job" buffer seconds after the hook
	// starts.
	snapshot, diagErr := githubactions.ReadRunnerDiag(d.runnerDir, githubactions.RunIdentityFromEnv(), time.Now())
	if diagErr == nil {
		if refs, ok := githubactions.RefsFromSetupJob(snapshot.SetupJob, snapshot.Worker, d.actionsCacheDir); ok {
			// Every action has its ref and SHA from the runner itself, so the cache has nothing left to
			// tell - provided the logs can be trusted. If not, the refs come from the folders on disk.
			if assessment := d.assess(refs, snapshot); assessment.JobTrusted {
				d.markVerification(refs, snapshot, assessment)
				return append(refs, unpairedCommits(refs, snapshot.Worker)...), nil
			}
		}
	}
	walked, err := cacheWalkDiscoverer{actionsCacheDir: d.actionsCacheDir}.Discover()
	if err != nil {
		return nil, err
	}
	if diagErr != nil || len(snapshot.Worker) == 0 {
		// The log format is the runner's, undocumented, and can change on upgrade. Curating by content
		// alone is what a step does, so every action is still decided - by content rather than by SHA.
		log.Warn(fmt.Sprintf("Cannot read the actions this job resolved from the runner's logs in %q - curating the action cache "+
			"without runner SHAs: %s", d.runnerDir, githubactions.QuoteForLog(errors.Join(diagErr, errNoLoggedActions(len(snapshot.Worker))).Error())))
		reason := githubactions.ReasonNoSHA
		if diagErr != nil {
			reason = githubactions.ReasonLogUntrusted
		}
		for i := range walked {
			walked[i].ContentReason = reason
		}
		return walked, nil
	}
	refs, unplaced := githubactions.AttachRunnerProvenance(walked, snapshot.SetupJob, snapshot.Worker)
	var missing []string
	for _, w := range unplaced {
		inCache := slices.ContainsFunc(walked, func(r githubactions.ActionRef) bool {
			return strings.EqualFold(r.Owner+"/"+r.Repo, w.Owner+"/"+w.Repo)
		})
		if !inCache {
			missing = append(missing, w.Owner+"/"+w.Repo+"@"+w.SHA)
		}
	}
	if len(missing) > 0 {
		return nil, errorutils.CheckErrorf("the runner fetched actions that are not in its action cache, so this job cannot be "+
			"reported as curated: %s", strings.Join(missing, ", "))
	}
	d.markVerification(refs, snapshot, d.assess(refs, snapshot))
	return append(refs, unpairedCommits(refs, snapshot.Worker)...), nil
}

// unpairedCommits returns, as refs decided by their SHA, the Worker-logged commits no ref holds under
// that same owner/repo, whatever the logs' trust. Which folder holds such a commit is unknown, but the
// commit is not, and each folder of its repository is decided on its own. A commit paired only under
// another name is among them: a forged setup line can hand one action another's SHA, and the commit
// must still be decided as the action the Worker fetched it for.
func unpairedCommits(refs []githubactions.ActionRef, worker []githubactions.WorkerAction) []githubactions.ActionRef {
	var unpaired []githubactions.ActionRef
	for _, w := range worker {
		held := func(r githubactions.ActionRef) bool {
			return strings.EqualFold(r.Owner, w.Owner) && strings.EqualFold(r.Repo, w.Repo) && strings.EqualFold(r.RunnerSHA, w.SHA)
		}
		if slices.ContainsFunc(refs, held) || slices.ContainsFunc(unpaired, held) {
			continue
		}
		unpaired = append(unpaired, githubactions.ActionRef{Owner: w.Owner, Repo: w.Repo, RunnerSHA: w.SHA,
			Verification: githubactions.VerifyLoggedSHA})
	}
	return unpaired
}

// assess runs the trust assessment over the snapshot and the refs found for it.
func (d runnerLogDiscoverer) assess(refs []githubactions.ActionRef, snapshot githubactions.DiagSnapshot) githubactions.LogAssessment {
	return githubactions.AssessLogs(githubactions.LogEvidence{
		Snapshot:     snapshot,
		Mode:         d.mode,
		Self:         d.self,
		GitHubHosted: d.githubHosted,
		Refs:         refs,
		TrustedCache: d.trustedCache,
	})
}

// trustedCache reports whether the cache entry the Worker loaded a from cannot have been changed by
// an earlier job: either the admin vouches for the cache, or this job's user neither owns nor can write it.
func (d runnerLogDiscoverer) trustedCache(a githubactions.WorkerAction) bool {
	return d.trustActionCache || githubactions.CacheEntryReadOnly(a.CacheDir, a)
}

// markVerification lets a ref be decided by its logged SHA only when the job's logs are trusted and
// no evidence rule downgraded that action; every other ref is compared by content, with a reason code
// for its Notes. A ref paired with a SHA the Worker logged under another name is compared too: names
// alone cannot tell a renamed repository from a setup line handing one action another's approved SHA.
func (d runnerLogDiscoverer) markVerification(refs []githubactions.ActionRef, snapshot githubactions.DiagSnapshot,
	assessment githubactions.LogAssessment) {
	if !assessment.JobTrusted {
		log.Warn("The runner's logs cannot be trusted for this job, so every action is verified by content instead of by the SHA " +
			"the runner logged: " + strings.Join(assessment.JobReasons, "; "))
	}
	for i := range refs {
		r := &refs[i]
		code := assessment.ActionReason[githubactions.AssessmentKey(r.Owner, r.Repo, r.RunnerSHA)]
		switch {
		case r.RunnerSHA == "":
			r.ContentReason = githubactions.ReasonNoSHA
			if assessment.JobTrusted {
				r.LoggedSHAs = loggedSHAs(snapshot.Worker, r.Owner, r.Repo)
			}
		case !assessment.JobTrusted:
			r.ContentReason = githubactions.ReasonLogUntrusted
		case code != "":
			r.ContentReason = code
		case !workerLogged(snapshot.Worker, r.Owner, r.Repo, r.RunnerSHA):
			r.ContentReason = githubactions.ReasonRenamed
		default:
			r.Verification = githubactions.VerifyLoggedSHA
			continue
		}
		log.Debug(fmt.Sprintf("github-actions curation: %s is verified by content: %s",
			githubactions.QuoteForLog(r.Owner+"/"+r.Repo), r.ContentReason))
	}
}

// loggedSHAs returns the distinct commits, lower-cased, the Worker log names for owner/repo.
func loggedSHAs(worker []githubactions.WorkerAction, owner, repo string) []string {
	var shas []string
	for _, w := range worker {
		sha := strings.ToLower(w.SHA)
		if strings.EqualFold(w.Owner, owner) && strings.EqualFold(w.Repo, repo) && !slices.Contains(shas, sha) {
			shas = append(shas, sha)
		}
	}
	return shas
}

// workerLogged reports whether the Worker log names owner/repo at sha itself, not under another name.
func workerLogged(worker []githubactions.WorkerAction, owner, repo, sha string) bool {
	return slices.ContainsFunc(worker, func(w githubactions.WorkerAction) bool {
		return strings.EqualFold(w.Owner, owner) && strings.EqualFold(w.Repo, repo) && strings.EqualFold(w.SHA, sha)
	})
}

func errNoLoggedActions(found int) error {
	if found > 0 {
		return nil
	}
	return errors.New("the Worker log names no action")
}
