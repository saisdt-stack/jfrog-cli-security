package curation

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"

	"github.com/jfrog/jfrog-cli-security/commands/curation/githubactions"
)

// actionDiscoverer lists the actions this job resolved, in the order the report shows them. A step
// on a GitHub-hosted runner can only walk the action cache; the job-started hook on a self-hosted
// runner can also read the runner's own logs, which name the SHA and cache source of each action.
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

// runnerLogDiscoverer lists the actions for the job-started hook on a self-hosted runner from what
// only the runner's own logs know: the SHA it fetched for each action and whether it came from the
// network or the runner's action cache. It walks the action cache only when the "Set up job" lines
// cannot account for every action, to recover the missing refs.
type runnerLogDiscoverer struct {
	runnerDir       string
	actionsCacheDir string
}

func (d runnerLogDiscoverer) Discover() ([]githubactions.ActionRef, error) {
	// First, before anything slow: the runner deletes the "Set up job" buffer seconds after the hook
	// starts.
	snapshot, diagErr := githubactions.ReadRunnerDiag(d.runnerDir)
	if diagErr == nil {
		if refs, ok := githubactions.RefsFromSetupJob(snapshot.SetupJob, snapshot.Worker, d.actionsCacheDir); ok {
			// Every action has its ref and SHA from the runner itself, and the decision is by SHA, so
			// the cache has nothing left to tell.
			return refs, nil
		}
	}
	walked, err := cacheWalkDiscoverer{actionsCacheDir: d.actionsCacheDir}.Discover()
	if err != nil {
		return nil, err
	}
	if diagErr != nil || len(snapshot.Worker) == 0 {
		// The log format is the runner's, undocumented, and can change on upgrade. Curating by content
		// alone is what a step does, so this costs the report its SHA and source columns, nothing more.
		log.Warn(fmt.Sprintf("Cannot read the actions this job resolved from the runner's logs in %q - curating the action cache "+
			"without runner SHAs: %v", d.runnerDir, errors.Join(diagErr, errNoLoggedActions(len(snapshot.Worker)))))
		return walked, nil
	}
	refs, unplaced := githubactions.AttachRunnerProvenance(walked, snapshot.SetupJob, snapshot.Worker)
	var missing []string
	for _, w := range unplaced {
		inCache := false
		for _, r := range walked {
			if strings.EqualFold(r.Owner+"/"+r.Repo, w.Owner+"/"+w.Repo) {
				inCache = true
				break
			}
		}
		if !inCache {
			missing = append(missing, w.Owner+"/"+w.Repo+"@"+w.SHA)
			continue
		}
		log.Warn(fmt.Sprintf("Cannot tell which ref of %s/%s the runner fetched as %s - that ref is curated by content, "+
			"and its runner SHA is left blank rather than guessed.", w.Owner, w.Repo, w.SHA))
	}
	if len(missing) > 0 {
		return nil, errorutils.CheckErrorf("the runner fetched actions that are not in its action cache, so this job cannot be "+
			"reported as curated: %s", strings.Join(missing, ", "))
	}
	return refs, nil
}

func errNoLoggedActions(found int) error {
	if found > 0 {
		return nil
	}
	return errors.New("the Worker log names no action")
}
