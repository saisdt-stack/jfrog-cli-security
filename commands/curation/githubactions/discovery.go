package githubactions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jfrog/jfrog-client-go/utils/log"
)

// What GitHub Actions sets on every runner.
const (
	// RunnerWorkspaceEnvVar is the workspace directory, e.g. /home/runner/work/<repo>, whose
	// sibling is _actions.
	RunnerWorkspaceEnvVar = "RUNNER_WORKSPACE"
	// WorkflowRefEnvVar is the running workflow's ref path, e.g.
	// "octocat/hello-world/.github/workflows/ci.yml@main".
	WorkflowRefEnvVar = "GITHUB_WORKFLOW_REF"
	// JobIDEnvVar is the running job's job_id - its key under `jobs:` in the workflow YAML.
	JobIDEnvVar = "GITHUB_JOB"
	// GithubRepoEnvVar is the repository running the job, as "<owner>/<repo>".
	GithubRepoEnvVar = "GITHUB_REPOSITORY"
)

// ActionRef is one resolved action instance found in the runner's action cache.
type ActionRef struct {
	Owner string
	Repo  string
	// Ref is taken verbatim from the cache directory name; it may be a SHA, tag or branch.
	Ref string
	// Path is the absolute path to _work/_actions/<Owner>/<Repo>/<Ref>.
	Path string
	// Subpaths holds every distinct subpath the job invoked this action through - a monorepo
	// action such as github/codeql-action can be used via several from one owner/repo/ref.
	Subpaths []string
	// Parent is the composite action that pulled this one in, "" when unattributed.
	Parent string
}

// DiscoverActionCache walks actionsCacheDir (the runner's _work/_actions root) exactly three
// levels deep (owner/repo/ref) and returns one ActionRef per leaf directory found.
//
// The runner downloads resolved actions here before the job's steps run, including transitive
// ones pulled in by another action's action.yml that never appear in the job's own workflow
// file - so the directory, not the YAML, is the account of what actually resolved.
//
// Entries not matching the owner/repo/ref shape are skipped rather than treated as errors: this
// walks a directory whose contents this code does not control.
func DiscoverActionCache(actionsCacheDir string) ([]ActionRef, error) {
	refs := []ActionRef{}

	ownerEntries, err := os.ReadDir(actionsCacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return refs, nil
		}
		return nil, fmt.Errorf("reading actions cache dir %q: %w", actionsCacheDir, err)
	}

	for _, ownerEntry := range ownerEntries {
		owner := ownerEntry.Name()
		ownerPath := filepath.Join(actionsCacheDir, owner)
		if !isDirFollowingLinks(ownerPath, "owner") {
			continue
		}

		repoEntries, err := os.ReadDir(ownerPath)
		if err != nil {
			log.Warn(fmt.Sprintf("github-actions curation: cannot list owner dir %q, so any action under it goes uncurated: %v", ownerPath, err))
			continue
		}
		for _, repoEntry := range repoEntries {
			repo := repoEntry.Name()
			repoPath := filepath.Join(ownerPath, repo)
			if !isDirFollowingLinks(repoPath, "repo") {
				continue
			}

			refEntries, err := os.ReadDir(repoPath)
			if err != nil {
				log.Warn(fmt.Sprintf("github-actions curation: cannot list repo dir %q, so any action under it goes uncurated: %v", repoPath, err))
				continue
			}
			for _, refEntry := range refEntries {
				refPath := filepath.Join(repoPath, refEntry.Name())
				if !isDirFollowingLinks(refPath, "ref") {
					continue
				}
				refs = append(refs, ActionRef{
					Owner: owner,
					Repo:  repo,
					Ref:   refEntry.Name(),
					Path:  refPath,
				})
			}
		}
	}
	// A populated cache that yields nothing is the signature of a layout this walk does not
	// understand - the symlinked-entry case was exactly that. Saying so once beats a per-entry
	// log nobody reads, and beats reporting a clean run over a set that was never examined.
	if len(refs) == 0 && len(ownerEntries) > 0 {
		log.Warn(fmt.Sprintf("github-actions curation: %q holds %d entries but none resolved to an <owner>/<repo>/<ref> action, "+
			"so nothing will be curated. The debug log names every entry that was skipped.", actionsCacheDir, len(ownerEntries)))
	}
	return refs, nil
}

// isDirFollowingLinks reports whether path is a directory, resolving a symlink to ask about its
// target rather than about the link itself.
//
// os.ReadDir's DirEntry.IsDir answers from the directory entry's own type - lstat semantics -
// so it reports false for a symlink to a directory. The runner materializes an entry that way
// whenever ACTIONS_RUNNER_SYMLINK_CACHED_ACTIONS is set and the action is already unpacked under
// ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE. Those actions execute like any other, so skipping them
// would curate a subset of the job while reporting a clean run over all of it.
func isDirFollowingLinks(path, level string) bool {
	info, err := os.Stat(path)
	if err != nil {
		log.Warn(fmt.Sprintf("github-actions curation: cannot resolve entry %q at %s level, so it goes uncurated: %v", path, level, err))
		return false
	}
	// Debug, not Warn: the runner writes a <ref>.completed watermark beside every extracted
	// action, so at ref level roughly half the entries are expected to land here.
	if !info.IsDir() {
		log.Debug(fmt.Sprintf("github-actions curation: skipping non-directory entry %q at %s level", path, level))
		return false
	}
	return true
}

// DefaultActionsCacheDir derives the runner's _actions cache path from RUNNER_WORKSPACE
// (<_work>/<repo>) - _actions is its sibling, i.e. dirname(RUNNER_WORKSPACE)/_actions.
func DefaultActionsCacheDir() (string, error) {
	runnerWorkspace := os.Getenv(RunnerWorkspaceEnvVar)
	if runnerWorkspace == "" {
		return "", fmt.Errorf("%s is not set - cannot derive the actions cache directory", RunnerWorkspaceEnvVar)
	}
	return filepath.Join(runnerWorkspace, "..", "_actions"), nil
}

// DefaultWorkflowFile derives the repo-relative path of the running workflow from
// GITHUB_WORKFLOW_REF, whose shape is "<owner>/<repo>/<path/to/workflow.yml>@<ref>".
//
// Returns "" - never an error - when the variable is unset or doesn't have that shape. An
// unrecognized value must not fail the command: the caller falls back to curating the action
// cache structure alone, without parent attribution.
func DefaultWorkflowFile() string {
	workflowRef := os.Getenv(WorkflowRefEnvVar)
	if workflowRef == "" {
		return ""
	}
	// The trailing "@<ref>" is a git ref and may itself contain "/" (refs/heads/my/branch).
	if atIdx := strings.LastIndex(workflowRef, "@"); atIdx >= 0 {
		workflowRef = workflowRef[:atIdx]
	}
	// Drop the leading "<owner>/<repo>/"; the rest is the path within the repository.
	segments := strings.SplitN(workflowRef, "/", 3)
	if len(segments) < 3 || segments[2] == "" {
		log.Debug(fmt.Sprintf("github-actions curation: %s=%q is not in <owner>/<repo>/<path>@<ref> form - cannot derive the workflow file from it", WorkflowRefEnvVar, os.Getenv(WorkflowRefEnvVar)))
		return ""
	}
	return segments[2]
}

// DefaultJobID returns the running job's job_id from GITHUB_JOB, or "" when unset.
func DefaultJobID() string {
	return os.Getenv(JobIDEnvVar)
}

// DefaultGithubRepo returns the running job's repository from GITHUB_REPOSITORY ("<owner>/<repo>"),
// or "" when unset.
func DefaultGithubRepo() string {
	return os.Getenv(GithubRepoEnvVar)
}

const (
	deliveryActionOwner = "jfrog"
	deliveryActionRepo  = "setup-jfrog-cli"
)

// ExcludeDeliveryAction drops jfrog/setup-jfrog-cli from refs, at any ref, so it is neither
// decided nor reported.
func ExcludeDeliveryAction(refs []ActionRef) []ActionRef {
	kept := make([]ActionRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Owner == deliveryActionOwner && ref.Repo == deliveryActionRepo {
			log.Debug(fmt.Sprintf("github-actions curation: skipping %s/%s@%s - it delivers and invokes this check rather than being subject to it", ref.Owner, ref.Repo, ref.Ref))
			continue
		}
		kept = append(kept, ref)
	}
	return kept
}
