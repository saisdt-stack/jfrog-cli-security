package curation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jfrog/jfrog-cli-security/commands/curation/githubactions"
)

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
	t.Run("verify when the hook rejects an action then its folder is removed and an approved one is kept", func(t *testing.T) {
		pinRunnerEnv(t, "octo/repo", "", "")
		t.Setenv(stepSummaryEnvVar, "")
		runnerDir, cacheDir := trustedHookJob(t)
		cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "").
			SetDecider(&scriptedDecider{rejected: []string{"actions/setup-node@v4"}})

		report, err := captureReport(t, cmd)

		require.Error(t, err)
		assert.ErrorContains(t, err, "actions/setup-node@v4")
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "setup-node", "v4")))
		assert.True(t, folderExists(t, filepath.Join(cacheDir, "actions", "setup-node")), "the repository folder itself stays")
		assert.True(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4")))
		assert.True(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4.completed")))
		assert.Equal(t, 1, strings.Count(report, "Removed"), "one warning lists what was removed: %s", report)
		assert.Contains(t, report, `"actions/setup-node@v4"`)
	})
	t.Run("verify when access is refused in the hook then every folder is removed", func(t *testing.T) {
		pinRunnerEnv(t, "octo/repo", "", "")
		t.Setenv(stepSummaryEnvVar, "")
		runnerDir, cacheDir := trustedHookJob(t)
		cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "").
			SetDecider(&probeDecider{denied: []string{"*"}})

		_, err := captureReport(t, cmd)

		require.ErrorIs(t, err, githubactions.ErrAccessDenied)
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4")))
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "setup-node", "v4")))
	})
	t.Run("verify when a step rejects an action then its folder is kept", func(t *testing.T) {
		pinRunnerEnv(t, testGithubRepo, "", "")
		spec := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}
		cmd := spec.newCommand(t, noWorkflowFile, "", &scriptedDecider{rejected: []string{"actions/checkout@v4"}})

		_, err := captureReport(t, cmd)

		require.Error(t, err)
		assert.True(t, folderExists(t, filepath.Join(cmd.actionsCacheDir, "actions", "checkout", "v4")))
	})
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

func TestCurationActionsCommand_Run_NeutralizesEverythingWithoutAVerdict(t *testing.T) {
	t.Run("verify when the hook cannot resolve the VCS repository then every folder is removed", func(t *testing.T) {
		pinRunnerEnv(t, "octo/repo", "", "")
		t.Setenv(stepSummaryEnvVar, "")
		runnerDir, cacheDir := trustedHookJob(t)
		cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "").
			SetVcsRepoResolver(&fixedResolver{err: errors.New("mapping API unreachable")}).SetDecider(&scriptedDecider{})

		_, err := captureReport(t, cmd)

		require.ErrorContains(t, err, "mapping API unreachable")
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4")))
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "setup-node", "v4")))
	})
	t.Run("verify when the hook's discovery fails then every action folder is removed", func(t *testing.T) {
		pinRunnerEnv(t, "octo/repo", "", "")
		t.Setenv(stepSummaryEnvVar, "")
		// setup-node's folder carries no watermark, so the walk cannot tell its ref: discovery refuses the job.
		runnerDir, cacheDir := hookRunner(t, map[string]string{
			"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) + savedLine("actions", "setup-node", testShaNode),
		}, []string{"actions/checkout/v4"})
		unmarked := filepath.Join(cacheDir, "actions", "setup-node", "v4")
		require.NoError(t, os.MkdirAll(unmarked, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(unmarked, "action.yml"), []byte("runs: {}"), 0o644))
		cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "").
			SetDecider(&scriptedDecider{})

		report, err := captureReport(t, cmd)

		require.ErrorContains(t, err, "cannot account for every entry")
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4")))
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4.completed")))
		assert.True(t, folderExists(t, cacheDir))
		assert.Contains(t, report, `"actions/checkout@v4"`)
	})
	t.Run("verify when a step cannot resolve the VCS repository then its folders are kept", func(t *testing.T) {
		pinRunnerEnv(t, testGithubRepo, "", "")
		spec := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}
		cmd := spec.newCommand(t, noWorkflowFile, "", &scriptedDecider{}).SetVcsRepoResolver(&fixedResolver{err: errors.New("unreachable")})

		_, err := captureReport(t, cmd)

		require.Error(t, err)
		assert.True(t, folderExists(t, filepath.Join(cmd.actionsCacheDir, "actions", "checkout", "v4")))
	})
	t.Run("verify when a pre continuing on error reaches no verdict then it warns the failure may be swallowed", func(t *testing.T) {
		pinRunnerEnv(t, testGithubRepo, "", "")
		t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
		t.Setenv(stepSummaryEnvVar, "")
		_, cacheDir := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}.build(t)
		cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(&scriptedDecider{}).
			SetVcsRepoResolver(&fixedResolver{err: errors.New("unreachable")}).SetFromPre(true)
		cmd.runnerDirFinder = nil

		report, err := captureReport(t, cmd)

		require.Error(t, err)
		assert.Contains(t, report, "may be swallowed")
		assert.False(t, folderExists(t, filepath.Join(cacheDir, "actions", "checkout", "v4")))
	})
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
