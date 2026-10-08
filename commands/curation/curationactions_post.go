package curation

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"

	"github.com/jfrog/jfrog-cli-security/commands/curation/githubactions"
)

const (
	// githubStateEnvVar names the file a pre appends name/value pairs to; the runner hands each one to
	// the same action's main and post as the environment variable STATE_<name>.
	githubStateEnvVar = "GITHUB_STATE"
	// decidedStateName is the state that carries the commits the pre decided to the post.
	decidedStateName = "JFROG_CURATION_DECIDED"
	// postStateEnvVar is how the runner hands that state to the post.
	postStateEnvVar = "STATE_" + decidedStateName
	// logsTrustedStateName is the state that tells the post whether the pre trusted the runner's logs:
	// "true" or "false".
	logsTrustedStateName = "JFROG_CURATION_LOGS_TRUSTED"
	postTrustEnvVar      = "STATE_" + logsTrustedStateName
	// maxDecidedState bounds the saved value: from about 100 KB of state the runner drops main's output,
	// and from about 300 KB it does not start post or main at all.
	maxDecidedState = 64 * 1024
	// fetchedLateNote marks a row the post decided whose commit the pre did not decide.
	fetchedLateNote = "fetched after the job started"
	// notBySHANote marks a row the post decided when the pre handed over no commit at all, so whether it
	// was fetched late is unknown.
	notBySHANote = "not decided by SHA in pre"
)

// SetFromPost is --from-post: the command runs in the post script of the action wrapping it and reports
// the actions the runner fetched after that action's pre decided the job's actions.
func (c *CurationActionsCommand) SetFromPost(fromPost bool) *CurationActionsCommand {
	c.fromPost = fromPost
	return c
}

// decidedKey is how the pre and the post name one decided commit: owner/repo@sha in lower case, or
// owner/repo@ref for a ref the logs gave no SHA, which no logged commit matches.
func decidedKey(owner, repo, version string) string {
	return strings.ToLower(owner + "/" + repo + "@" + version)
}

// saveDecided appends to GITHUB_STATE the commits decided in this pre, so the post decides only what the
// runner fetched later, and whether this pre trusted the runner's logs, so the post can say what its own
// reading of them is worth. Outside a runner nothing is saved; a set too large to save is left out, and
// the post then decides every commit the runner logged.
func saveDecided(refs []githubactions.ActionRef, logsTrusted bool) (err error) {
	path := os.Getenv(githubStateEnvVar)
	if path == "" {
		return nil
	}
	// #nosec G302 G304 G703 -- the runner names this file for this step and reads it back; it holds action names and SHAs
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return errorutils.CheckError(err)
	}
	defer func() {
		err = errors.Join(err, errorutils.CheckError(file.Close()))
	}()
	if _, err = fmt.Fprintf(file, "%s=%t\n", logsTrustedStateName, logsTrusted); err != nil {
		return errorutils.CheckError(err)
	}
	var keys []string
	for _, r := range refs {
		version := r.RunnerSHA
		if version == "" {
			version = r.Ref
		}
		keys = append(keys, decidedKey(r.Owner, r.Repo, version))
	}
	slices.Sort(keys)
	value := strings.Join(slices.Compact(keys), "\n")
	if len(value) > maxDecidedState {
		log.Info(fmt.Sprintf("The %d actions decided here are too many to hand to this action's post, so the post will decide every "+
			"action the runner logged.", len(refs)))
		return nil
	}
	// The heredoc form, since the value spans lines; a random delimiter cannot appear in it.
	delimiter := "ghadelimiter_" + rand.Text()
	_, err = fmt.Fprintf(file, "%s<<%s\n%s\n%s\n", decidedStateName, delimiter, value, delimiter)
	return errorutils.CheckError(err)
}

// runPost decides, from the post of the action wrapping the command, every commit the runner's Worker
// log names that the pre did not decide: actions fetched after the job started, such as those inside a
// local composite action. It cannot undo what those actions did, so nothing is removed; it makes them
// visible and fails the post for one curation does not approve.
//
// A best-effort report: the job's steps ran before the post and could have written the runner's logs,
// and a runner that cannot be found or read leaves nothing to check, which is warned about rather than
// failed. Each commit is decided by its SHA, never by content: the runner's copy may have changed.
func (c *CurationActionsCommand) runPost(ctx context.Context) error {
	runnerDir := c.runnerDir
	if runnerDir == "" {
		dir, err := c.findRunnerDir()
		if err != nil {
			log.Warn("Cannot find the runner from this step's process tree, so the actions it fetched after the job started cannot be checked: " +
				githubactions.QuoteForLog(err.Error()))
			return nil
		}
		runnerDir = dir
	}
	snapshot, err := githubactions.ReadRunnerDiag(runnerDir, githubactions.RunIdentityFromEnv(), time.Now())
	if err != nil {
		log.Warn("Cannot read the runner's logs, so the actions it fetched after the job started cannot be checked: " +
			githubactions.QuoteForLog(err.Error()))
		return nil
	}
	if os.Getenv(postTrustEnvVar) != "true" {
		log.Warn("The pre did not trust this job's runner logs, or did not say, and this check reads logs that the job's steps could " +
			"have changed since: an empty result here is not proof that no action was fetched late.")
	}
	saved, ok := os.LookupEnv(postStateEnvVar)
	if !ok {
		log.Warn("The actions decided by the pre were not saved, so every action the runner logged is decided again.")
	}
	decided := map[string]bool{}
	note := notBySHANote
	for _, key := range strings.Fields(saved) {
		decided[strings.ToLower(key)] = true
		if _, version, _ := strings.Cut(key, "@"); isObjectID(version) {
			note = fetchedLateNote
		}
	}
	var late []githubactions.ActionRef
	for _, w := range snapshot.Worker {
		key := decidedKey(w.Owner, w.Repo, w.SHA)
		if decided[key] {
			continue
		}
		decided[key] = true
		// The SHA is the row's ref, so the decider does not label it an unpaired logged commit: the post
		// pairs nothing, and its rows say only why they were decided here.
		late = append(late, githubactions.ActionRef{Owner: w.Owner, Repo: w.Repo, Ref: w.SHA, RunnerSHA: w.SHA,
			Verification: githubactions.VerifyLoggedSHA})
	}
	if len(late) == 0 {
		log.Info("The runner fetched no action after the job started.")
		return nil
	}

	artifactoryVcsRepo, err := c.resolveArtifactoryVcsRepo(ctx)
	if err != nil {
		return err
	}
	decider, err := c.resolveDecider()
	if err != nil {
		return err
	}
	outcome := c.decideAll(ctx, decider, artifactoryVcsRepo, late)
	if outcome.accessErr != nil {
		return outcome.accessErr
	}
	for i := range outcome.rows {
		row := &outcome.rows[i]
		if row.Notes == "" {
			row.Notes = note
		} else {
			row.Notes += "; " + note
		}
	}
	log.Info(fmt.Sprintf("GitHub Actions the runner logged that its pre did not decide by SHA:\n%s", githubactions.RenderReportTable(outcome.rows, false)))
	if recordErr := appendStepSummary(curatedActions(outcome.rows, false, nil)); recordErr != nil {
		log.Warn(fmt.Sprintf("Failed to record the GitHub Actions curation summary - the report above is the complete result: %v", recordErr))
	}
	return errors.Join(outcome.decideErr(), notApprovedError(outcome.decidedRows()))
}

// isObjectID reports whether s is a full 40- or 64-hex git object ID.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}
