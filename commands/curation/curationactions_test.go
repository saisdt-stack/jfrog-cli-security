package curation

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/common/cliutils"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jfrog/jfrog-cli-security/commands/curation/githubactions"
)

const (
	curationActionsFixture = "../../tests/testdata/projects/githubactions/curation-project"
	testGithubRepo         = "my-org/my-repo"
	// derivedWorkflowRef is the shape GITHUB_WORKFLOW_REF carries on a runner; its path component
	// is repo-relative, so it resolves against the working directory.
	derivedWorkflowRef = "my-org/my-repo/.github/workflows/ci.yml@refs/heads/main"
)

// fixtureCacheEntries is what the curation-project fixture's _actions tree holds.
var fixtureCacheEntries = []string{"actions/checkout@v4", "github/codeql-action@v3", "some-org/transitive-action@v1"}

// scriptedDecider stands in for the real decision service. It fails to decide the keys in
// undecidable, rejects the keys in rejected, approves everything else, and records what it was
// asked so a test can assert on curation SCOPE rather than only on the command's exit status.
// Keys are "owner/repo@ref". undecidable wins over rejected: no decision is not a decision.
//
// Run decides actions concurrently, so the recording is locked, and the order of asked is the
// order decisions happened to start in - assert it as a set, not a sequence.
type scriptedDecider struct {
	rejected    []string
	undecidable []string
	// silentlyUndetermined actions come back Undetermined with no error - a decider that gives up
	// without saying why.
	silentlyUndetermined []string

	mu    sync.Mutex
	asked []string
	// vcsRepos records the Artifactory VCS repository each decision was made under, so tests can
	// prove the resolved value actually reached the decider rather than being computed and dropped.
	vcsRepos []string
}

func (d *scriptedDecider) Decide(_ context.Context, artifactoryVcsRepo string, ref githubactions.ActionRef) (githubactions.ActionCurationResult, error) {
	key := ref.Owner + "/" + ref.Repo + "@" + ref.Ref
	d.mu.Lock()
	d.asked = append(d.asked, key)
	d.vcsRepos = append(d.vcsRepos, artifactoryVcsRepo)
	d.mu.Unlock()
	if slices.Contains(d.undecidable, key) {
		return githubactions.ActionCurationResult{}, errors.New("decision service unavailable")
	}
	if slices.Contains(d.silentlyUndetermined, key) {
		return githubactions.ActionCurationResult{Status: githubactions.ActionUndetermined}, nil
	}
	if slices.Contains(d.rejected, key) {
		return githubactions.ActionCurationResult{Status: githubactions.ActionRejected, Notes: "rejected in test"}, nil
	}
	return githubactions.ActionCurationResult{Status: githubactions.ActionApproved}, nil
}

// fixedResolver returns a known key, or err when the mapping API is meant to be unreachable,
// and records what it was asked about.
type fixedResolver struct {
	repo       string
	err        error
	askedAbout []string
}

func (f *fixedResolver) Resolve(_ context.Context, githubRepo string) (string, error) {
	f.askedAbout = append(f.askedAbout, githubRepo)
	if f.err != nil {
		return "", f.err
	}
	return f.repo, nil
}

// pinRunnerEnv fixes every GitHub environment variable the command reads, so a test's result
// never depends on whether it happens to be running inside GitHub Actions - where all of them
// are set, and would otherwise leak into these tests. Pass "" to represent unset.
func pinRunnerEnv(t *testing.T, githubRepo, workflowRef, jobID string) {
	t.Helper()
	t.Setenv(githubactions.GithubRepoEnvVar, githubRepo)
	t.Setenv(githubactions.WorkflowRefEnvVar, workflowRef)
	t.Setenv(githubactions.JobIDEnvVar, jobID)
}

// workflowFileMode selects what SetWorkflowFile points at, if anything.
type workflowFileMode int

const (
	noWorkflowFile       workflowFileMode = iota // omit the flag; resolution falls to the environment
	writtenWorkflowFile                          // the ci.yml runnerSpec wrote into the working directory
	fixtureWorkflowFile                          // the curation-project fixture's own ci.yml
	missingWorkflowFile                          // a path that does not exist
	relativeWorkflowFile                         // a relative path - SetWorkflowFile must be absolute
)

// runnerSpec describes the runner state a test starts from, as data rather than as setup code:
// what sits in the action cache, and what the workspace holds at .github/workflows/ci.yml.
type runnerSpec struct {
	// fixtureCache seeds the cache from the curation-project fixture's _actions tree.
	fixtureCache bool
	// cacheDirs are "owner/repo/ref" entries to create on top of that. Each is an action root, so
	// build writes the <ref>.completed watermark a runner would leave beside it - without one,
	// discovery cannot read a ref from the entry and refuses to curate the cache.
	cacheDirs []string
	// cacheFiles are files to write inside the cache, keyed by "owner/repo/ref/name".
	cacheFiles map[string]string
	// workflowYAML, when set, is written to <workingDir>/.github/workflows/ci.yml - the path
	// derivedWorkflowRef resolves to.
	workflowYAML string
	// unreadableWorkflow strips read permission from that file, for the case where the path
	// resolves and opening it still fails.
	unreadableWorkflow bool
}

func (s runnerSpec) build(t *testing.T) (workingDir, actionsCacheDir string) {
	t.Helper()
	workingDir = t.TempDir()
	actionsCacheDir = filepath.Join(t.TempDir(), "_actions")
	require.NoError(t, os.MkdirAll(actionsCacheDir, 0755))
	if s.fixtureCache {
		require.NoError(t, os.CopyFS(actionsCacheDir, os.DirFS(filepath.Join(curationActionsFixture, "_work", "_actions"))))
	}
	for _, dir := range s.cacheDirs {
		path := filepath.Join(actionsCacheDir, filepath.FromSlash(dir))
		require.NoError(t, os.MkdirAll(path, 0755))
		require.NoError(t, os.WriteFile(path+".completed", []byte("ts"), 0600))
	}
	for name, content := range s.cacheFiles {
		path := filepath.Join(actionsCacheDir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	}
	if s.workflowYAML != "" {
		workflowsDir := filepath.Join(workingDir, ".github", "workflows")
		require.NoError(t, os.MkdirAll(workflowsDir, 0755))
		workflowPath := filepath.Join(workflowsDir, "ci.yml")
		require.NoError(t, os.WriteFile(workflowPath, []byte(s.workflowYAML), 0600))
		if s.unreadableWorkflow {
			require.NoError(t, os.Chmod(workflowPath, 0000))
			// Restored so the temp dir can be cleaned up on platforms that need read access.
			t.Cleanup(func() { _ = os.Chmod(workflowPath, 0600) })
		}
	}
	return workingDir, actionsCacheDir
}

// newCommand builds the command every Run test exercises, wiring SetWorkflowFile per mode.
func (s runnerSpec) newCommand(t *testing.T, mode workflowFileMode, jobID string, decider githubactions.ActionCurationDecider) *CurationActionsCommand {
	t.Helper()
	workingDir, actionsCacheDir := s.build(t)
	cmd := NewCurationActionsCommand().
		SetWorkingDir(workingDir).
		SetActionsCacheDir(actionsCacheDir).
		SetDecider(decider)
	switch mode {
	case writtenWorkflowFile:
		cmd.SetWorkflowFile(filepath.Join(workingDir, ".github", "workflows", "ci.yml"))
	case fixtureWorkflowFile:
		// Absolute on purpose: a relative SetWorkflowFile resolves against the working directory,
		// which here is a temp dir, not the package the fixture path is written relative to.
		abs, err := filepath.Abs(filepath.Join(curationActionsFixture, ".github", "workflows", "ci.yml"))
		require.NoError(t, err)
		cmd.SetWorkflowFile(abs)
	case missingWorkflowFile:
		cmd.SetWorkflowFile(filepath.Join(workingDir, "no-such-workflow.yml"))
	case relativeWorkflowFile:
		cmd.SetWorkflowFile(filepath.Join(".github", "workflows", "ci.yml"))
	case noWorkflowFile:
	}
	if jobID != "" {
		cmd.SetJobID(jobID)
	}
	return cmd
}

func TestCurationActionsCommand_Run_CurationScope(t *testing.T) {
	// What actually gets decided, per resolution path. The invariant across every row: the
	// runner's cache IS this job's action list, so every entry in it is decided regardless of
	// what the workflow file does or does not explain.
	const twoJobs = "jobs:\n" +
		"  build:\n    steps:\n      - uses: actions/checkout@v4\n" +
		"  publish:\n    steps:\n      - uses: some-other-org/publisher@v9\n"

	tests := []struct {
		name        string
		spec        runnerSpec
		mode        workflowFileMode
		jobID       string
		envWorkflow string
		wantAsked   []string
	}{
		{
			name:      "verify when no workflow file is identified then every cache entry is still decided",
			spec:      runnerSpec{fixtureCache: true},
			wantAsked: fixtureCacheEntries,
		},
		{
			name:      "verify when an entry appears in no workflow file then it is still decided",
			spec:      runnerSpec{cacheDirs: []string{"some-org/unreferenced/v9"}},
			wantAsked: []string{"some-org/unreferenced@v9"},
		},
		{
			name: "verify when only GITHUB_WORKFLOW_REF identifies the workflow then attribution still runs",
			spec: runnerSpec{fixtureCache: true, workflowYAML: "jobs:\n  build:\n    steps:\n" +
				"      - uses: actions/checkout@v4\n      - uses: github/codeql-action/analyze@v3\n"},
			jobID:       "build",
			envWorkflow: derivedWorkflowRef,
			wantAsked:   fixtureCacheEntries,
		},
		{
			name:        "verify when a sibling job declares an action then it is decided but not attributed",
			spec:        runnerSpec{cacheDirs: []string{"actions/checkout/v4", "some-other-org/publisher/v9"}, workflowYAML: twoJobs},
			jobID:       "build",
			envWorkflow: derivedWorkflowRef,
			wantAsked:   []string{"actions/checkout@v4", "some-other-org/publisher@v9"},
		},
		{
			name: "verify when the cache holds the action delivering this check then it is curated too",
			spec: runnerSpec{
				cacheDirs:    []string{"jfrog/setup-jfrog-cli/v4", "actions/checkout/v4"},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: jfrog/setup-jfrog-cli@v4\n      - uses: actions/checkout@v4\n",
			},
			mode:      writtenWorkflowFile,
			jobID:     "build",
			wantAsked: []string{"actions/checkout@v4", "jfrog/setup-jfrog-cli@v4"},
		},
		{
			// The reusable-workflow case. Attributing from the file's other jobs once curated an
			// action only they declared, and dropped one this job really used.
			name: "verify when the workflow does not declare this job then this cache is curated and other jobs ignored",
			spec: runnerSpec{
				cacheDirs:    []string{"actions/checkout/v4", "actions/setup-node/v4"},
				workflowYAML: "jobs:\n  some-other-job:\n    steps:\n      - uses: actions/checkout@v4\n",
			},
			mode:      writtenWorkflowFile,
			jobID:     "the-job-this-command-runs-in",
			wantAsked: []string{"actions/checkout@v4", "actions/setup-node@v4"},
		},
		{
			// uses: ./... is read from the workspace, so there is nothing to walk outward from - the
			// entry is unattributable, and must still be curated. This covers the case where the runner
			// had already resolved the child by the time this command ran; the case where it has not is
			// the "job declares a local composite action" row of the coverage caveat table.
			name: "verify when a local composite action's child is already in the cache then it is still decided",
			spec: runnerSpec{
				cacheDirs:    []string{"actions/setup-node/v4"},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: ./.github/actions/setup\n",
			},
			mode:      writtenWorkflowFile,
			jobID:     "build",
			wantAsked: []string{"actions/setup-node@v4"},
		},
		{
			// Duplicate mapping keys: accepted by GitHub's runner, rejected by yaml.v3.
			name: "verify when a composite action.yml cannot be parsed then its child is still decided",
			spec: runnerSpec{
				cacheDirs: []string{"actions/setup-node/v4", "some-org/wrapper/v1"},
				cacheFiles: map[string]string{
					"some-org/wrapper/v1/action.yml": "name: w\nname: w\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-node@v4\n",
				},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: some-org/wrapper@v1\n",
			},
			mode:      writtenWorkflowFile,
			jobID:     "build",
			wantAsked: []string{"actions/setup-node@v4", "some-org/wrapper@v1"},
		},
		{
			// The delivery action is a parent like any other, and is curated like any other rather than
			// being filtered out of the table after attribution has already pointed at it.
			name: "verify when the action delivering this check pulls in another action then both are decided",
			spec: runnerSpec{
				cacheDirs: []string{"jfrog/setup-jfrog-cli/v4", "some-org/pulled-by-delivery/v1"},
				cacheFiles: map[string]string{
					"jfrog/setup-jfrog-cli/v4/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: some-org/pulled-by-delivery@v1\n",
				},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: jfrog/setup-jfrog-cli@v4\n",
			},
			mode:      writtenWorkflowFile,
			jobID:     "build",
			wantAsked: []string{"jfrog/setup-jfrog-cli@v4", "some-org/pulled-by-delivery@v1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, tt.envWorkflow, "")
			decider := &scriptedDecider{}

			require.NoError(t, tt.spec.newCommand(t, tt.mode, tt.jobID, decider).Run())

			assert.ElementsMatch(t, tt.wantAsked, decider.asked,
				"every entry the runner resolved into this job's cache must be decided")
		})
	}
}

func TestCurationActionsCommand_Run_Gate(t *testing.T) {
	// The job continues only when every action is explicitly Approved. Rejected and Undetermined each
	// fail it, are each reported as their own row, and the error names what failed and why.
	twoActions := runnerSpec{cacheDirs: []string{"actions/checkout/v4", "actions/setup-node/v4"}}

	tests := []struct {
		name  string
		spec  runnerSpec
		mode  workflowFileMode
		jobID string
		// rejected, undecidable and silentlyUndetermined script the decider, keyed "owner/repo@ref".
		rejected             []string
		undecidable          []string
		silentlyUndetermined []string
		wantErr              bool
		wantErrContains      []string // each must appear in the error
		wantErrNotContains   []string // none may appear in the error
		// wantNoVerdict: the run stops before deciding anything - nothing asked, no report, no summary.
		wantNoVerdict bool
		// wantRows is the report's row count per status; a status left out must have no row.
		wantRows map[githubactions.ActionCurationStatus]int
	}{
		{
			// SetJobID is what puts this in ATTRIBUTED mode - a workflow file alone is not enough,
			// since attribution also needs to know which job it is describing.
			name:     "verify when every action is approved then the command succeeds",
			spec:     runnerSpec{fixtureCache: true},
			mode:     fixtureWorkflowFile,
			jobID:    "build",
			wantRows: map[githubactions.ActionCurationStatus]int{githubactions.ActionApproved: 3},
		},
		{
			name:            "verify when an action is rejected then the command fails and the error names it",
			spec:            runnerSpec{fixtureCache: true},
			mode:            fixtureWorkflowFile,
			rejected:        []string{"some-org/transitive-action@v1"},
			wantErr:         true,
			wantErrContains: []string{`"some-org/transitive-action@v1": status "Rejected"`},
			wantRows:        map[githubactions.ActionCurationStatus]int{githubactions.ActionApproved: 2, githubactions.ActionRejected: 1},
		},
		{
			// It will execute, so failing to explain why it is there is no grounds for skipping it.
			name:     "verify when an entry no workflow explains is rejected then the command fails",
			spec:     runnerSpec{fixtureCache: true, cacheDirs: []string{"some-other-org/unexplained-action/v9"}},
			mode:     fixtureWorkflowFile,
			jobID:    "build",
			rejected: []string{"some-other-org/unexplained-action@v9"},
			wantErr:  true,
			wantRows: map[githubactions.ActionCurationStatus]int{githubactions.ActionApproved: 3, githubactions.ActionRejected: 1},
		},
		{
			// No action is rejected here: the cache holds an entry the walk cannot resolve to an
			// identity, and curating the rest would report a clean run over an action whose status
			// was never established. feature/my-branch carries no watermark while v4 does.
			name: "verify when a cache entry cannot be accounted for then the command fails without deciding",
			spec: runnerSpec{
				cacheDirs: []string{"actions/checkout/v4"},
				// Written through cacheFiles so the directory exists with no watermark beside it.
				cacheFiles: map[string]string{"actions/checkout/feature/my-branch/action.yml": "runs:\n  using: node20\n"},
			},
			mode:          noWorkflowFile,
			wantErr:       true,
			wantNoVerdict: true,
		},
		{
			// Dropping the unattributable entry once turned a Rejected action into a green build.
			name: "verify when the workflow parses to no action reference then a rejected cache entry still fails",
			spec: runnerSpec{
				cacheDirs:    []string{"evil-org/backdoor/v1"},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: ./.github/actions/setup\n",
			},
			mode:     writtenWorkflowFile,
			jobID:    "build",
			rejected: []string{"evil-org/backdoor@v1"},
			wantErr:  true,
			wantRows: map[githubactions.ActionCurationStatus]int{githubactions.ActionRejected: 1},
		},
		{
			name:        "verify when every decision fails then the error names each action and the cause",
			spec:        twoActions,
			undecidable: []string{"actions/checkout@v4", "actions/setup-node@v4"},
			wantErr:     true,
			wantErrContains: []string{"deciding curation status for", "actions/checkout@v4", "actions/setup-node@v4",
				"decision service unavailable"},
			wantRows: map[githubactions.ActionCurationStatus]int{githubactions.ActionUndetermined: 2},
		},
		{
			name:               "verify when only one decision fails then the error names it alone and the other is still reported",
			spec:               twoActions,
			undecidable:        []string{"actions/setup-node@v4"},
			wantErr:            true,
			wantErrContains:    []string{"actions/setup-node@v4"},
			wantErrNotContains: []string{"actions/checkout@v4"},
			wantRows:           map[githubactions.ActionCurationStatus]int{githubactions.ActionApproved: 1, githubactions.ActionUndetermined: 1},
		},
		{
			name:        "verify when actions are approved, rejected and undecidable then each is its own row and the error names both failures",
			spec:        runnerSpec{cacheDirs: []string{"actions/checkout/v4", "evil-org/backdoor/v1", "flaky-org/remote/v1"}},
			rejected:    []string{"evil-org/backdoor@v1"},
			undecidable: []string{"flaky-org/remote@v1"},
			wantErr:     true,
			wantErrContains: []string{`"evil-org/backdoor@v1": status "Rejected"`, "flaky-org/remote@v1",
				"decision service unavailable"},
			// An Undetermined action is explained once, by its cause.
			wantErrNotContains: []string{`status "Undetermined"`},
			wantRows: map[githubactions.ActionCurationStatus]int{
				githubactions.ActionApproved: 1, githubactions.ActionRejected: 1, githubactions.ActionUndetermined: 1},
		},
		{
			// Only an explicit Approved may clear the gate.
			name:                 "verify when a decider returns Undetermined without an error then the command still fails",
			spec:                 runnerSpec{cacheDirs: []string{"actions/checkout/v4", "quiet-org/action/v1"}},
			silentlyUndetermined: []string{"quiet-org/action@v1"},
			wantErr:              true,
			wantErrContains:      []string{`"quiet-org/action@v1": status "Undetermined"`},
			wantRows:             map[githubactions.ActionCurationStatus]int{githubactions.ActionApproved: 1, githubactions.ActionUndetermined: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			// Recording is a no-op unless this is set.
			summaryDir := t.TempDir()
			t.Setenv(coreutils.SummaryOutputDirPathEnv, summaryDir)
			decider := &scriptedDecider{rejected: tt.rejected, undecidable: tt.undecidable, silentlyUndetermined: tt.silentlyUndetermined}

			report, err := captureReport(t, tt.spec.newCommand(t, tt.mode, tt.jobID, decider))

			assert.Equal(t, tt.wantErr, err != nil, "Run() error = %v", err)
			for _, want := range tt.wantErrContains {
				assert.ErrorContains(t, err, want)
			}
			for _, notWant := range tt.wantErrNotContains {
				assert.NotContains(t, fmt.Sprint(err), notWant)
			}
			entries, readErr := os.ReadDir(summaryDir)
			require.NoError(t, readErr)
			if tt.wantNoVerdict {
				assert.Empty(t, decider.asked, "nothing may be decided when the cache cannot be accounted for")
				assert.NotContains(t, report, "GitHub Actions Curation Report")
				assert.Empty(t, entries)
				return
			}
			for _, status := range []githubactions.ActionCurationStatus{githubactions.ActionApproved, githubactions.ActionRejected, githubactions.ActionUndetermined} {
				assert.Equal(t, tt.wantRows[status], strings.Count(report, "| "+string(status)+" |"), "%s rows in:\n%s", status, report)
			}
			if len(tt.undecidable) > 0 {
				assert.Contains(t, report, "decision service unavailable", "an Undetermined row must carry its cause")
			}
			assert.NotEmpty(t, entries, "the job summary is recorded whatever the verdict")
		})
	}
}

func TestCurationActionsCommand_Run_WorkflowFileResolution(t *testing.T) {
	// The branches of parseWorkflowUses, exercised through Run in the shape a real runner
	// produces: _actions populated, workspace empty because checkout has not run.
	oneAction := []string{"actions/checkout/v4"}

	tests := []struct {
		name            string
		spec            runnerSpec
		mode            workflowFileMode
		workflowRef     string
		wantAsked       []string
		wantErrContains string
	}{
		{
			// GITHUB_WORKFLOW_REF is always set on a runner, so without this fallback every job
			// that did not pass SetWorkflowFile would fail here.
			name:        "verify when the derived workflow path is absent then curation falls back to structure-only",
			spec:        runnerSpec{cacheDirs: oneAction},
			workflowRef: derivedWorkflowRef,
			wantAsked:   []string{"actions/checkout@v4"},
		},
		{
			name:      "verify when an explicit workflow path is absent then curation falls back to structure-only",
			spec:      runnerSpec{cacheDirs: oneAction},
			mode:      missingWorkflowFile,
			wantAsked: []string{"actions/checkout@v4"},
		},
		{
			// The one thing that still fails: a malformed flag, rejected before anything is read.
			// SetWorkflowFile is an explicit, single-file assertion; only the derived path
			// (GITHUB_WORKFLOW_REF) resolves against the working directory.
			name:            "verify when an explicit workflow path is relative then the command fails",
			spec:            runnerSpec{cacheDirs: oneAction},
			mode:            relativeWorkflowFile,
			wantErrContains: "must be an absolute path",
		},
		{
			// A workflow this parser cannot read costs attribution and nothing else, so the cache
			// is still curated in full.
			name:        "verify when the derived workflow file is malformed then curation falls back to structure-only",
			spec:        runnerSpec{cacheDirs: oneAction, workflowYAML: "jobs:\n\t- this is not valid yaml\n"},
			workflowRef: derivedWorkflowRef,
			wantAsked:   []string{"actions/checkout@v4"},
		},
		{
			// Naming the file asserts it exists, not that this parser can read it - so the same
			// divergence degrades the same way whichever route resolved the path.
			name:      "verify when an explicit workflow file is malformed then curation falls back to structure-only",
			spec:      runnerSpec{cacheDirs: oneAction, workflowYAML: "jobs:\n\t- this is not valid yaml\n"},
			mode:      writtenWorkflowFile,
			wantAsked: []string{"actions/checkout@v4"},
		},
		{
			name:        "verify when the derived workflow file is valid then attribution runs normally",
			spec:        runnerSpec{cacheDirs: oneAction, workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n"},
			workflowRef: derivedWorkflowRef,
			wantAsked:   []string{"actions/checkout@v4"},
		},
		{
			// The path resolves and opening it still fails - a permission error rather than an absent
			// file. It reaches the command as neither ErrNotExist nor a parse error, which is the one
			// route that used to abort the run; curation is the job, so it degrades like the rest.
			name: "verify when the derived workflow file is unreadable then curation falls back to structure-only",
			spec: runnerSpec{cacheDirs: oneAction, workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n",
				unreadableWorkflow: true},
			workflowRef: derivedWorkflowRef,
			wantAsked:   []string{"actions/checkout@v4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.spec.unreadableWorkflow && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("file permissions cannot deny the read here (Windows only toggles read-only; root ignores modes)")
			}
			pinRunnerEnv(t, testGithubRepo, tt.workflowRef, "build")
			decider := &scriptedDecider{}

			err := tt.spec.newCommand(t, tt.mode, "", decider).Run()

			if tt.wantErrContains != "" {
				assert.ErrorContains(t, err, tt.wantErrContains)
				assert.Empty(t, decider.asked)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.wantAsked, decider.asked,
				"every entry in the cache must be curated, whichever branch resolution took")
		})
	}
}

func TestCurationActionsCommand_Run_ArtifactoryVcsRepoResolution(t *testing.T) {
	spec := runnerSpec{cacheDirs: []string{"actions/checkout/v4", "actions/setup-node/v4"}}

	tests := []struct {
		name         string
		envRepo      string
		flagRepo     string
		resolverRepo string
		resolverErr  error
		// wantAskedAbout is what the mapping API was called with - once per run, never per action.
		wantAskedAbout []string
		// wantVcsRepos is the repository that reached each decision, so a resolved value that is
		// computed and then dropped fails here rather than passing silently.
		wantVcsRepos    []string
		wantErrContains []string
	}{
		{
			name:           "verify when GITHUB_REPOSITORY is set then the resolved repository reaches every decision",
			envRepo:        testGithubRepo,
			resolverRepo:   "my-org-github-remote",
			wantAskedAbout: []string{testGithubRepo},
			wantVcsRepos:   []string{"my-org-github-remote", "my-org-github-remote"},
		},
		{
			name:           "verify when the repository is set explicitly then it overrides the environment",
			envRepo:        testGithubRepo,
			flagRepo:       "flag-org/flag-repo",
			resolverRepo:   "resolved",
			wantAskedAbout: []string{"flag-org/flag-repo"},
			wantVcsRepos:   []string{"resolved", "resolved"},
		},
		{
			name:            "verify when repository resolution fails then the command fails and nothing is decided",
			envRepo:         testGithubRepo,
			resolverErr:     errors.New("mapping service unavailable"),
			wantAskedAbout:  []string{testGithubRepo},
			wantErrContains: []string{"resolving the Artifactory VCS repository governing", "mapping service unavailable"},
		},
		{
			name:            "verify when no GitHub repository is known then the error names the variable that supplies it",
			envRepo:         "", // GITHUB_REPOSITORY unset, e.g. a local run
			wantErrContains: []string{githubactions.GithubRepoEnvVar},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, tt.envRepo, "", "")
			resolver := &fixedResolver{repo: tt.resolverRepo, err: tt.resolverErr}
			decider := &scriptedDecider{}
			cmd := spec.newCommand(t, noWorkflowFile, "", decider).SetVcsRepoResolver(resolver)
			if tt.flagRepo != "" {
				cmd.SetGithubRepo(tt.flagRepo)
			}

			err := cmd.Run()

			assert.Equal(t, tt.wantAskedAbout, resolver.askedAbout, "resolution happens once per run, not once per action")
			if len(tt.wantErrContains) > 0 {
				for _, want := range tt.wantErrContains {
					assert.ErrorContains(t, err, want)
				}
				assert.Empty(t, decider.asked, "nothing may be decided when the governing repository is unknown")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantVcsRepos, decider.vcsRepos, "the resolved repository must reach every decision")
		})
	}
}

func TestCurationActionsCommand_Run_EmptyCacheFailsBeforeResolving(t *testing.T) {
	pinRunnerEnv(t, testGithubRepo, "", "")
	resolver := &fixedResolver{repo: "unused"}

	err := runnerSpec{}.newCommand(t, noWorkflowFile, "", &scriptedDecider{}).
		SetVcsRepoResolver(resolver).Run()

	assert.ErrorContains(t, err, githubactions.RunnerWorkspaceEnvVar,
		"the failure must name the variable the cache location comes from")
	assert.Empty(t, resolver.askedAbout, "a cache that cannot be read must fail before any mapping call")
}

// captureReport runs cmd with the logger redirected, and returns everything it wrote. The
// report and its caveat only exist as log output, so that is where a test has to read them.
func captureReport(t *testing.T, cmd *CurationActionsCommand) (report string, err error) {
	t.Helper()
	var buf bytes.Buffer
	original := log.Logger
	log.SetLogger(log.NewLogger(log.INFO, &buf))
	defer log.SetLogger(original)
	err = cmd.Run()
	return buf.String(), err
}

func TestCurationActionsCommand_Run_CoverageCaveatPerResolutionPath(t *testing.T) {
	// The caveat has to track what was actually knowable on each path, not merely appear.
	tests := []struct {
		name            string
		spec            runnerSpec
		mode            workflowFileMode
		jobID           string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "verify when the workflow declares no local action then the report claims full coverage",
			spec: runnerSpec{
				cacheDirs:    []string{"actions/checkout/v4"},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n",
			},
			mode:            writtenWorkflowFile,
			jobID:           "build",
			wantNotContains: []string{"Not covered"},
		},
		{
			name: "verify when no workflow file is available then the caveat is unconditional",
			// The ordinary case on a runner: the check runs before the checkout, so the file is
			// not on disk and the command cannot tell whether a local action is declared.
			spec:         runnerSpec{cacheDirs: []string{"actions/checkout/v4"}},
			mode:         noWorkflowFile,
			wantContains: []string{"Local composite actions (uses: ./...) are not curated"},
		},
		{
			name: "verify when the workflow does not declare the job then the caveat is unconditional",
			// Attribution failed for a different reason, but the command knows exactly as little.
			spec: runnerSpec{
				cacheDirs:    []string{"actions/checkout/v4"},
				workflowYAML: "jobs:\n  publish:\n    steps:\n      - uses: actions/checkout@v4\n",
			},
			mode:         writtenWorkflowFile,
			jobID:        "build",
			wantContains: []string{"Local composite actions (uses: ./...) are not curated"},
		},
		{
			name: "verify when a composite declares a local step then the caveat names it and its declarer",
			spec: runnerSpec{
				cacheDirs: []string{"some-org/wrapper/v1"},
				cacheFiles: map[string]string{
					"some-org/wrapper/v1/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: ./scripts/build\n",
				},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: some-org/wrapper@v1\n",
			},
			mode:         writtenWorkflowFile,
			jobID:        "build",
			wantContains: []string{"Not covered", "./scripts/build", "declared by some-org/wrapper@v1"},
		},
		{
			name: "verify when attribution is unavailable then a composite's local step is not detected at all",
			// Detection needs the walk, and the walk needs a workflow file. Without one the
			// command genuinely does not know - which is why that caveat is unconditional rather
			// than a list, and why it must not name a path it never read.
			spec: runnerSpec{
				cacheDirs: []string{"some-org/wrapper/v1"},
				cacheFiles: map[string]string{
					"some-org/wrapper/v1/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: ./scripts/build\n",
				},
			},
			mode:            noWorkflowFile,
			wantContains:    []string{"Local composite actions (uses: ./...) are not curated"},
			wantNotContains: []string{"./scripts/build"},
		},
		{
			name: "verify when a sibling job declares the local action then this job does not claim it",
			// Each job runs on its own runner. A local step in another job says nothing about the
			// coverage of this one, and naming it here would be a false lead.
			spec: runnerSpec{
				cacheDirs: []string{"actions/checkout/v4"},
				workflowYAML: "jobs:\n" +
					"  build:\n    steps:\n      - uses: actions/checkout@v4\n" +
					"  publish:\n    steps:\n      - uses: ./.github/actions/release\n",
			},
			mode:            writtenWorkflowFile,
			jobID:           "build",
			wantNotContains: []string{"Not covered", "./.github/actions/release"},
		},
		{
			// The gap this caveat exists for: the job declares a local composite action, and the action
			// it pulls in is not in the cache, because the runner cannot resolve a local action's own
			// references until the workspace is checked out. Nothing can be decided, so the command is
			// right to pass; the report must not let an all-Approved table stand as the whole account.
			name: "verify when the job declares a local composite action then the caveat names it",
			spec: runnerSpec{
				cacheDirs:    []string{"actions/checkout/v4"},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: ./.github/actions/setup\n",
			},
			mode:         writtenWorkflowFile,
			jobID:        "build",
			wantContains: []string{"./.github/actions/setup", "not curated here"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")

			report, err := captureReport(t, tt.spec.newCommand(t, tt.mode, tt.jobID, &scriptedDecider{}))

			require.NoError(t, err)
			for _, want := range tt.wantContains {
				assert.Contains(t, report, want)
			}
			for _, notWant := range tt.wantNotContains {
				assert.NotContains(t, report, notWant)
			}
		})
	}
}

func TestCurationActionsCommand_Run_ErrorHandlingHookAppliesToFailuresNotOutcomes(t *testing.T) {
	// errorutils.CheckError is a hook, not a wrapper: it is the identity function until
	// JFROG_CLI_ERROR_HANDLING=panic replaces it with one that panics at the error site, which
	// is how this repository gets a stack trace pointing at an origin. That makes WHERE it is
	// applied the whole decision - an operational failure should reach it, while a normal
	// outcome expressed as an error must not, or debugging with that flag set turns working
	// runs into panics.
	origCheckError := errorutils.CheckError
	errorutils.CheckError = func(err error) error {
		if err != nil {
			panic(err)
		}
		return nil
	}
	defer func() { errorutils.CheckError = origCheckError }()

	tests := []struct {
		name            string
		spec            runnerSpec
		mode            workflowFileMode
		rejected        []string
		undecidable     []string
		unresolvableVcs bool
		wantPanic       bool
		wantErr         bool
	}{
		{
			// The gate's verdict. The command worked exactly as designed; the error is only how
			// a rejection reaches the exit code.
			name:     "verify when an action is rejected then the verdict is not treated as a failure",
			spec:     runnerSpec{cacheDirs: []string{"evil-org/backdoor/v1"}},
			mode:     noWorkflowFile,
			rejected: []string{"evil-org/backdoor@v1"},
			wantErr:  true,
		},
		{
			// The ordinary case on a runner: no checkout yet, so the workflow file is absent and
			// the run degrades to structure-only. Nothing failed.
			name: "verify when the workflow file is absent then degrading is not treated as a failure",
			spec: runnerSpec{cacheDirs: []string{"actions/checkout/v4"}},
			mode: missingWorkflowFile,
		},
		{
			// A remote seam. Reaching no verdict is an anticipated condition of calling a
			// service, and the fail-open / fail-close setting is what decides its consequence -
			// so it must not crash, least of all on a transient fault.
			name:        "verify when a decision cannot be reached then the remote failure is not treated as a failure of this command",
			spec:        runnerSpec{cacheDirs: []string{"actions/checkout/v4"}},
			mode:        noWorkflowFile,
			undecidable: []string{"actions/checkout@v4"},
			wantErr:     true,
		},
		{
			// The other remote seam, for the same reason.
			name:            "verify when the vcs repository cannot be resolved then the remote failure does not crash",
			spec:            runnerSpec{cacheDirs: []string{"actions/checkout/v4"}},
			mode:            noWorkflowFile,
			unresolvableVcs: true,
			wantErr:         true,
		},
		{
			// An operational failure: the cache holds an entry that cannot be resolved to an
			// action, so the run cannot be reported as curated. This one must reach the hook.
			name: "verify when the cache cannot be accounted for then the failure reaches the hook",
			spec: runnerSpec{
				cacheDirs:  []string{"actions/checkout/v4"},
				cacheFiles: map[string]string{"actions/checkout/feature/my-branch/action.yml": "runs:\n  using: node20\n"},
			},
			mode:      noWorkflowFile,
			wantPanic: true,
		},
		{
			// A malformed flag, rejected before anything is read.
			name:      "verify when the workflow path is relative then the usage error reaches the hook",
			spec:      runnerSpec{cacheDirs: []string{"actions/checkout/v4"}},
			mode:      relativeWorkflowFile,
			wantPanic: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			run := func() error {
				cmd := tt.spec.newCommand(t, tt.mode, "",
					&scriptedDecider{rejected: tt.rejected, undecidable: tt.undecidable})
				if tt.unresolvableVcs {
					cmd.SetVcsRepoResolver(&fixedResolver{err: errors.New("mapping API unreachable")})
				}
				return cmd.Run()
			}

			if tt.wantPanic {
				assert.Panics(t, func() { _ = run() })
				return
			}
			var err error
			require.NotPanics(t, func() { err = run() })
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// probeDecider approves or refuses actions on a script while measuring concurrency: how many
// decisions were in flight at once, and how many were made.
type probeDecider struct {
	// hold is how long a decision takes, per action; nil means immediate.
	hold func(ref githubactions.ActionRef) time.Duration
	// barrier, when set, holds every decision until that many are in flight at once, then releases
	// them all - proof of parallelism that does not depend on sleeps overlapping.
	barrier     int32
	release     chan struct{}
	releaseOnce sync.Once
	// denied actions fail as an access failure, keyed "owner/repo@ref"; "*" denies every action.
	denied []string
	// rejected actions are Rejected, keyed "owner/repo@ref".
	rejected []string

	calls       atomic.Int32
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (p *probeDecider) Decide(_ context.Context, _ string, ref githubactions.ActionRef) (githubactions.ActionCurationResult, error) {
	p.calls.Add(1)
	now := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		seen := p.maxInFlight.Load()
		if now <= seen || p.maxInFlight.CompareAndSwap(seen, now) {
			break
		}
	}
	if p.hold != nil {
		time.Sleep(p.hold(ref))
	}
	if p.barrier > 0 {
		if now >= p.barrier {
			p.releaseOnce.Do(func() { close(p.release) })
		}
		select {
		case <-p.release:
		case <-time.After(5 * time.Second):
			// Only stops a serialized runner from hanging the test; the maxInFlight assertion fails it.
		}
	}
	key := ref.Owner + "/" + ref.Repo + "@" + ref.Ref
	if slices.Contains(p.denied, "*") || slices.Contains(p.denied, key) {
		return githubactions.ActionCurationResult{}, fmt.Errorf("fetching %s: %w", key, githubactions.ErrAccessDenied)
	}
	if slices.Contains(p.rejected, key) {
		return githubactions.ActionCurationResult{Status: githubactions.ActionRejected, Notes: "rejected in test"}, nil
	}
	return githubactions.ActionCurationResult{Status: githubactions.ActionApproved}, nil
}

// orderedActions is a cache of n actions that discovery lists in name order: a-org, b-org, ...
func orderedActions(n int) (runnerSpec, []string) {
	var spec runnerSpec
	var keys []string
	for i := range n {
		owner := string(rune('a'+i)) + "-org"
		spec.cacheDirs = append(spec.cacheDirs, owner+"/act/v1")
		keys = append(keys, owner+"/act@v1")
	}
	return spec, keys
}

func TestCurationActionsCommand_Run_StopsOnAccessFailure(t *testing.T) {
	tests := []struct {
		name    string
		actions int // size of the orderedActions cache: a-org/act@v1, b-org/act@v1, ...
		threads int
		// denied actions fail with ErrAccessDenied; "*" denies every action. rejected are Rejected.
		denied   []string
		rejected []string
		// wantAccessErr: the run stops on the access failure; otherwise every action is decided.
		wantAccessErr bool
		wantMaxCalls  int32 // upper bound on decisions made when the run stops
	}{
		{name: "verify when one action is refused access with a single thread then no later action is decided and nothing is reported",
			actions: 5, threads: 1, denied: []string{"b-org/act@v1"}, wantAccessErr: true, wantMaxCalls: 2},
		{name: "verify when access is refused with several threads then the run stops well short of every action",
			// Up to threads decisions are in flight when the first refusal lands, and each other worker
			// may begin one more before it sees the stop; 20 would mean nothing stopped.
			actions: 20, threads: 3, denied: []string{"*"}, wantAccessErr: true, wantMaxCalls: 6},
		{name: "verify when an action is rejected then the run does not stop and every action is decided",
			actions: 5, threads: 1, rejected: []string{"b-org/act@v1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			summaryDir := t.TempDir()
			t.Setenv(coreutils.SummaryOutputDirPathEnv, summaryDir)
			spec, _ := orderedActions(tt.actions)
			decider := &probeDecider{denied: tt.denied, rejected: tt.rejected}

			report, err := captureReport(t, spec.newCommand(t, noWorkflowFile, "", decider).SetParallelRequests(tt.threads))

			require.Error(t, err)
			assert.Equal(t, tt.wantAccessErr, errors.Is(err, githubactions.ErrAccessDenied), "Run() error = %v", err)
			if tt.wantAccessErr {
				assert.LessOrEqual(t, decider.calls.Load(), tt.wantMaxCalls, "the run did not stop on the access failure")
				assert.Equal(t, 1, strings.Count(err.Error(), githubactions.ErrAccessDenied.Error()), "one access failure is reported, not one per action: %v", err)
				assert.NotContains(t, report, "GitHub Actions Curation Report", "a run stopped part-way must not report")
				entries, readErr := os.ReadDir(summaryDir)
				require.NoError(t, readErr)
				assert.Empty(t, entries, "a run stopped part-way must not record a job summary")
				return
			}
			assert.Equal(t, int32(tt.actions), decider.calls.Load(), "every action must be decided after a rejection")
			assert.Equal(t, len(tt.rejected), strings.Count(report, "| Rejected |"))
			assert.Equal(t, tt.actions-len(tt.rejected), strings.Count(report, "| Approved |"))
		})
	}
}

func TestCurationActionsCommand_Run_ParallelDecisions(t *testing.T) {
	tests := []struct {
		name        string
		threads     int
		wantMaxOpen int32
	}{
		{name: "verify when threads is set then no more decisions than that run at once", threads: 2, wantMaxOpen: 2},
		{name: "verify when threads is not set then the CLI default bounds the decisions in flight", threads: 0, wantMaxOpen: int32(cliutils.Threads)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			spec, _ := orderedActions(8)
			decider := &probeDecider{barrier: tt.wantMaxOpen, release: make(chan struct{})}

			err := spec.newCommand(t, noWorkflowFile, "", decider).SetParallelRequests(tt.threads).Run()

			require.NoError(t, err)
			assert.Equal(t, int32(8), decider.calls.Load())
			assert.Equal(t, tt.wantMaxOpen, decider.maxInFlight.Load(), "decisions in flight at once, want exactly the thread bound")
		})
	}
}

func TestCurationActionsCommand_Run_ReportOrderIsDiscoveryOrder(t *testing.T) {
	pinRunnerEnv(t, testGithubRepo, "", "")
	spec, _ := orderedActions(5)
	// The first action is the slowest, so with every action in flight at once they finish last-first.
	decider := &probeDecider{hold: func(ref githubactions.ActionRef) time.Duration {
		return time.Duration('f'-rune(ref.Owner[0])) * 15 * time.Millisecond
	}}

	report, err := captureReport(t, spec.newCommand(t, noWorkflowFile, "", decider).SetParallelRequests(5))

	require.NoError(t, err)
	last := -1
	for _, owner := range []string{"a-org", "b-org", "c-org", "d-org", "e-org"} {
		idx := strings.Index(report, "| "+owner+"/act |")
		require.GreaterOrEqual(t, idx, 0, "%s missing from the report:\n%s", owner, report)
		assert.Greater(t, idx, last, "%s is out of discovery order:\n%s", owner, report)
		last = idx
	}
}

func TestCurationActionsCommand_Run_RequiresAServerWithoutATestDecider(t *testing.T) {
	tests := []struct {
		name          string
		serverDetails *config.ServerDetails
	}{
		{name: "verify when no server details are set then the command reports no JFrog server"},
		// What the CLI resolves when jf config holds no server: empty, not nil.
		{name: "verify when the server details are empty then the command reports no JFrog server", serverDetails: &config.ServerDetails{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			workingDir, actionsCacheDir := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}.build(t)

			err := NewCurationActionsCommand().SetWorkingDir(workingDir).SetActionsCacheDir(actionsCacheDir).
				SetServerDetails(tt.serverDetails).Run()

			assert.ErrorContains(t, err, "no JFrog server is configured")
		})
	}
}

// actionTarGz builds a tar.gz shaped like Artifactory's VCS archive: files under one top directory.
func actionTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "action-top/", Mode: 0o755, Typeflag: tar.TypeDir}))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := files[name]
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "action-top/" + name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// snapshotTree records every file below dir with its content, so a test can prove it is unchanged.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		snapshot[path] = string(content)
		return err
	}))
	return snapshot
}

func TestCurationActionsCommand_Run_ContentMismatchIsReportedAndFailsTheGate(t *testing.T) {
	// The real decider, so the mismatch comes from comparing the served archive with the runner's
	// copy. Full-SHA refs keep git refs out of it: ref resolution is covered by the decider's tests.
	const (
		shaOne   = "1111111111111111111111111111111111111111"
		shaTwo   = "2222222222222222222222222222222222222222"
		shaThree = "3333333333333333333333333333333333333333"
		vcsRepo  = "github-vcs"
	)
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(coreutils.SummaryOutputDirPathEnv, t.TempDir())
	runnerYAML := map[string]string{"one": "name: one\n", "two": "name: two\n", "three": "name: three\n"}
	served := map[string][]byte{
		"downloadCommit/" + vcsRepo + "/acme/one/" + shaOne:     actionTarGz(t, map[string]string{"action.yml": runnerYAML["one"]}),
		"downloadCommit/" + vcsRepo + "/acme/two/" + shaTwo:     actionTarGz(t, map[string]string{"action.yml": "name: moved\n"}),
		"downloadCommit/" + vcsRepo + "/acme/three/" + shaThree: actionTarGz(t, map[string]string{"action.yml": runnerYAML["three"]}),
	}
	var mu sync.Mutex
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/artifactory/api/vcs/")
		mu.Lock()
		requested = append(requested, key)
		mu.Unlock()
		archive, ok := served[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	decider, err := githubactions.NewArtifactoryActionCurationDecider(&config.ServerDetails{
		ArtifactoryUrl: server.URL + "/artifactory/",
		AccessToken:    "test-token",
	})
	require.NoError(t, err)
	spec := runnerSpec{
		cacheDirs: []string{"acme/one/" + shaOne, "acme/two/" + shaTwo, "acme/three/" + shaThree},
		cacheFiles: map[string]string{
			"acme/one/" + shaOne + "/action.yml":     runnerYAML["one"],
			"acme/two/" + shaTwo + "/action.yml":     runnerYAML["two"],
			"acme/three/" + shaThree + "/action.yml": runnerYAML["three"],
		},
	}
	workingDir, actionsCacheDir := spec.build(t)
	before := snapshotTree(t, actionsCacheDir)
	cmd := NewCurationActionsCommand().
		SetWorkingDir(workingDir).
		SetActionsCacheDir(actionsCacheDir).
		SetVcsRepoResolver(&fixedResolver{repo: vcsRepo}).
		SetDecider(decider)

	report, err := captureReport(t, cmd)

	const mismatch = "not able to decide since content is mismatched (action.yml differs)"
	assert.ElementsMatch(t, slices.Collect(maps.Keys(served)), requested, "every action must be decided")
	assert.Equal(t, 2, strings.Count(report, "| Approved |"), report)
	assert.Equal(t, 1, strings.Count(report, "| Rejected |"), report)
	require.Error(t, err, "a content mismatch must fail the gate")
	assert.ErrorContains(t, err, "acme/two@"+shaTwo)
	assert.ErrorContains(t, err, mismatch)
	assert.NotContains(t, err.Error(), "acme/one", "an approved action must not be named by the gate")
	assert.NotContains(t, err.Error(), "acme/three", "an approved action must not be named by the gate")
	assert.Equal(t, before, snapshotTree(t, actionsCacheDir), "the runner's action cache was modified")
}

func TestCurationActionsCommand_Run_AttributesOnlyFromTheCommitBeingRun(t *testing.T) {
	// A persistent self-hosted runner keeps the workspace between jobs, so the workflow file found
	// there may be from another commit an earlier job checked out. It describes this job only when
	// the checkout is at the commit this job runs.
	const runSHA = "ac28ebad35b91c09ec81aab0ceeb2517d0c96145"
	const otherSHA = "2fe6323c9d78da97e1a802f768ec291d1e59d794"
	tests := []struct {
		name         string
		gitHead      string // "" leaves no checkout in the workspace
		wantAttribed bool
	}{
		{name: "verify when the checkout is at the commit being run then the workflow file is used", gitHead: runSHA, wantAttribed: true},
		{name: "verify when the checkout is at another commit then the workflow file is ignored", gitHead: otherSHA},
		{name: "verify when the workspace has no checkout then the workflow file is ignored"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, derivedWorkflowRef, "build")
			t.Setenv(githubactions.GithubSHAEnvVar, runSHA)
			spec := runnerSpec{
				cacheDirs:    []string{"actions/checkout/v4"},
				workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: ./.github/actions/stale-only\n",
			}
			workingDir, cacheDir := spec.build(t)
			if tt.gitHead != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(workingDir, ".git"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(workingDir, ".git", "HEAD"), []byte(tt.gitHead+"\n"), 0o644))
			}
			cmd := NewCurationActionsCommand().SetWorkingDir(workingDir).SetActionsCacheDir(cacheDir).SetDecider(&scriptedDecider{})
			report, err := captureReport(t, cmd)
			require.NoError(t, err)
			if tt.wantAttribed {
				assert.Contains(t, report, "./.github/actions/stale-only")
				return
			}
			assert.NotContains(t, report, "./.github/actions/stale-only")
			assert.Contains(t, report, "Local composite actions (uses: ./...) are not curated")
		})
	}
}

func TestNotApprovedErrorQuotesWhatItRepeats(t *testing.T) {
	// The action and ref of an unpaired row, and an Undetermined row's Notes, can carry text read from
	// the runner's logs; on stdout a line starting with :: is a workflow command.
	err := notApprovedError([]githubactions.ActionReportRow{
		{Action: "evil/x\n::error::a", Ref: "v1\r\n::error::b", Status: string(githubactions.ActionRejected), Notes: "n\n::error::c"},
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "\n::error::")
	assert.NotContains(t, err.Error(), "\r")
	assert.Contains(t, err.Error(), `"evil/x::error::a@v1::error::b": status "Rejected" - "n::error::c"`)
}

func folderExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestCurationActionsCommand_Run_NeutralizesWhatIsNotApproved(t *testing.T) {
	tests := []struct {
		name string
		// hook runs Run as the job-started hook over trustedHookJob's runner; false runs it as a plain step
		// over a runnerSpec cache. Both hold actions/checkout/v4 and actions/setup-node/v4, unless diag
		// sets another layout.
		hook     bool
		rejected []string // keys the scriptedDecider rejects
		denied   bool     // a probeDecider refuses access to every action instead
		vcsErr   bool     // the VCS repository resolver fails, so no verdict is reached
		// diag and cacheDirs, when diag is set, replace trustedHookJob's layout (hookRunner's arguments).
		diag      map[string]string
		cacheDirs []string
		// unmarked is a cache-relative action folder created holding an action.yml and no watermark, so
		// the walk cannot tell its ref and discovery cannot account for it.
		unmarked        string
		wantErrContains string
		wantRemoved     []string // cache-relative paths gone after Run
		wantKept        []string // cache-relative paths still there ("." is the cache itself)
		// wantRemovedWarning are the quoted names the single "Removed ..." warning must list; nil: no warning.
		wantRemovedWarning []string
	}{
		{
			name:            "verify when the hook rejects an action then its folder is removed and an approved one is kept",
			hook:            true,
			rejected:        []string{"actions/setup-node@v4"},
			wantErrContains: "actions/setup-node@v4",
			wantRemoved:     []string{"actions/setup-node/v4", "actions/setup-node/v4.completed"},
			// The repository folder itself stays.
			wantKept:           []string{"actions/setup-node", "actions/checkout/v4", "actions/checkout/v4.completed"},
			wantRemovedWarning: []string{`"actions/setup-node@v4"`},
		},
		{
			name:               "verify when access is refused in the hook then every folder is removed",
			hook:               true,
			denied:             true,
			wantErrContains:    githubactions.ErrAccessDenied.Error(),
			wantRemoved:        []string{"actions/checkout/v4", "actions/setup-node/v4"},
			wantRemovedWarning: []string{`"actions/checkout@v4"`, `"actions/setup-node@v4"`},
		},
		{
			name:               "verify when the hook cannot resolve the VCS repository then every folder is removed",
			hook:               true,
			vcsErr:             true,
			wantErrContains:    "mapping API unreachable",
			wantRemoved:        []string{"actions/checkout/v4", "actions/setup-node/v4"},
			wantRemovedWarning: []string{`"actions/checkout@v4"`, `"actions/setup-node@v4"`},
		},
		{
			// No setup line names setup-node and its folder carries no watermark: discovery refuses the job.
			name: "verify when the hook's discovery fails then every action folder is removed",
			hook: true,
			diag: map[string]string{
				"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) + savedLine("actions", "setup-node", testShaNode),
			},
			cacheDirs:          []string{"actions/checkout/v4"},
			unmarked:           "actions/setup-node/v4",
			wantErrContains:    "cannot account for every entry",
			wantRemoved:        []string{"actions/checkout/v4", "actions/checkout/v4.completed", "actions/setup-node/v4"},
			wantKept:           []string{"."},
			wantRemovedWarning: []string{`"actions/checkout@v4"`},
		},
		{
			// The Worker saved testShaV4 under newowner/tool, which no folder is paired with, while oldowner/tool/v1
			// holds that same commit: the renamed repository's folder would stay runnable if only the unpaired
			// commit were removed. The unpaired ref has no ref name, so it is rejected as "newowner/tool@".
			name: "verify when the hook rejects an unpaired commit then the folder of the same commit under another name is removed",
			hook: true,
			diag: map[string]string{
				"pages/a_1.log": setupLine("oldowner", "tool", "v1", testShaV4) + setupLine("actions", "checkout", "v4", testShaV3),
				"Worker_20261005-050652-utc.log": workerStart + savedLine("newowner", "tool", testShaV4) +
					savedLine("actions", "checkout", testShaV3),
			},
			cacheDirs:          []string{"oldowner/tool/v1", "actions/checkout/v4"},
			rejected:           []string{"newowner/tool@"},
			wantErrContains:    "newowner/tool",
			wantRemoved:        []string{"oldowner/tool/v1", "oldowner/tool/v1.completed"},
			wantKept:           []string{"actions/checkout/v4", "actions/checkout/v4.completed"},
			wantRemovedWarning: []string{`"oldowner/tool@v1"`},
		},
		{
			name:            "verify when a step rejects an action then its folder is kept",
			rejected:        []string{"actions/checkout@v4"},
			wantErrContains: "actions/checkout@v4",
			wantKept:        []string{"actions/checkout/v4", "actions/setup-node/v4"},
		},
		{
			name:            "verify when a step cannot resolve the VCS repository then its folders are kept",
			vcsErr:          true,
			wantErrContains: "mapping API unreachable",
			wantKept:        []string{"actions/checkout/v4", "actions/setup-node/v4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, "octo/repo", "", "")
			t.Setenv(stepSummaryEnvVar, "")
			var decider githubactions.ActionCurationDecider = &scriptedDecider{rejected: tt.rejected}
			if tt.denied {
				decider = &probeDecider{denied: []string{"*"}}
			}
			var cmd *CurationActionsCommand
			if tt.hook {
				var runnerDir, cacheDir string
				if tt.diag != nil {
					runnerDir, cacheDir = hookRunner(t, tt.diag, tt.cacheDirs)
				} else {
					runnerDir, cacheDir = trustedHookJob(t)
				}
				if tt.unmarked != "" {
					folder := filepath.Join(cacheDir, filepath.FromSlash(tt.unmarked))
					require.NoError(t, os.MkdirAll(folder, 0o755))
					require.NoError(t, os.WriteFile(filepath.Join(folder, "action.yml"), []byte("runs: {}"), 0o644))
				}
				cmd = NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).
					SetCallerMode(githubactions.ModeHook, "").SetDecider(decider)
			} else {
				cmd = runnerSpec{cacheDirs: []string{"actions/checkout/v4", "actions/setup-node/v4"}}.newCommand(t, noWorkflowFile, "", decider)
			}
			if tt.vcsErr {
				cmd.SetVcsRepoResolver(&fixedResolver{err: errors.New("mapping API unreachable")})
			}

			report, err := captureReport(t, cmd)

			require.ErrorContains(t, err, tt.wantErrContains)
			if tt.denied {
				assert.ErrorIs(t, err, githubactions.ErrAccessDenied)
			}
			for _, p := range tt.wantRemoved {
				assert.False(t, folderExists(t, filepath.Join(cmd.actionsCacheDir, filepath.FromSlash(p))), "%s must be removed", p)
			}
			for _, p := range tt.wantKept {
				assert.True(t, folderExists(t, filepath.Join(cmd.actionsCacheDir, filepath.FromSlash(p))), "%s must be kept", p)
			}
			wantWarnings := 0
			if len(tt.wantRemovedWarning) > 0 {
				wantWarnings = 1
			}
			assert.Equal(t, wantWarnings, strings.Count(report, "Removed the runner's copy"), "one warning lists what was removed: %s", report)
			for _, name := range tt.wantRemovedWarning {
				assert.Contains(t, report, name)
			}
		})
	}
}

func TestCurationActionsCommand_Run_FromPreWarnsWhenTheFailureMayBeSwallowed(t *testing.T) {
	ourStep := func(continueOnError string) string {
		return `, "timeline": {"id": "a"}, "steps": [` +
			`{"reference": {"type": "repository", "repositoryType": "GitHub", "name": "jfrog/curate", "ref": "v1"}` + continueOnError + `}, ` +
			`{"reference": {"type": "repository", "repositoryType": "GitHub", "name": "actions/checkout", "ref": "v4"}}]`
	}
	tests := []struct {
		name        string
		job         string
		invisible   bool
		wantWarning bool
	}{
		{name: "verify when our step continues on error then the warning says the removal is what stops the action",
			job: ourStep(`, "continueOnError": {"bool": true}`), wantWarning: true},
		{name: "verify when our step does not continue on error then there is no such warning", job: ourStep("")},
		{name: "verify when the runner cannot be found then the warning is given", invisible: true, wantWarning: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
			t.Setenv(githubactions.RunnerEnvironmentEnvVar, "github-hosted")
			t.Setenv(stepSummaryEnvVar, "")
			runnerDir, cacheDir := hookRunnerForJob(t, tt.job, map[string]string{
				"pages/a_1.log": setupLine("jfrog", "curate", "v1", testShaSelf) + setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": workerStart + savedLine("jfrog", "curate", testShaSelf) +
					savedLine("actions", "checkout", testShaV4),
			}, []string{"jfrog/curate/v1", "actions/checkout/v4"})
			cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(&scriptedDecider{rejected: []string{"actions/checkout@v4"}}).
				SetFromPre(true)
			cmd.runnerDirFinder = func() (string, error) { return runnerDir, nil }
			if tt.invisible {
				cmd.runnerDirFinder = func() (string, error) { return "", githubactions.ErrRunnerNotVisible }
			}

			report, err := captureReport(t, cmd)

			require.Error(t, err)
			assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4")))
			assert.True(t, folderExists(t, filepath.Join(cacheDir, "jfrog", "curate", "v1")))
			assert.Equal(t, tt.wantWarning, strings.Contains(report, "may be swallowed"), "report: %s", report)
		})
	}
}

func TestCurationActionsCommand_Run_FromPreKeepsTheRepositoryFolderOfAnUndeterminedAction(t *testing.T) {
	// The hosted e2e case: an Undetermined private action whose ref folder holds a sub-path action.
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
	t.Setenv(githubactions.RunnerEnvironmentEnvVar, "github-hosted")
	t.Setenv(stepSummaryEnvVar, "")
	t.Setenv(githubStateEnvVar, "")
	const probe = "dattathallam/probe-runner-hook-sha"
	runnerDir, cacheDir := hookRunnerForJob(t, jobFields("jfrog/curate", probe), map[string]string{
		"pages/a_1.log": setupLine("jfrog", "curate", "v1", testShaSelf) + setupLine("dattathallam", "probe-runner-hook-sha", "e2e-v2", testShaV4),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("jfrog", "curate", testShaSelf) +
			savedLine("dattathallam", "probe-runner-hook-sha", testShaV4),
	}, []string{"jfrog/curate/v1", probe + "/e2e-v2"})
	repoDir := filepath.Join(cacheDir, "dattathallam", "probe-runner-hook-sha")
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "e2e-v2", "e2e-victim"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "e2e-v2", "e2e-victim", "action.yml"), []byte("runs: {}"), 0o644))
	cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(&scriptedDecider{undecidable: []string{probe + "@e2e-v2"}}).
		SetFromPre(true)
	cmd.runnerDirFinder = func() (string, error) { return runnerDir, nil }

	report, err := captureReport(t, cmd)

	require.Error(t, err)
	assert.False(t, folderExists(t, filepath.Join(repoDir, "e2e-v2")))
	assert.False(t, folderExists(t, filepath.Join(repoDir, "e2e-v2.completed")))
	assert.True(t, folderExists(t, repoDir), "the repository folder itself stays")
	assert.True(t, folderExists(t, filepath.Join(cacheDir, "jfrog", "curate", "v1")))
	assert.Contains(t, report, `: "`+probe+`@e2e-v2"`+"\n", "the warning names only the removed ref folder: %s", report)
}
