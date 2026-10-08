package curation

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jfrog/jfrog-cli-security/commands/curation/githubactions"
)

// fixedDiscoverer returns refs and err as given, so a test controls exactly what Run curates.
type fixedDiscoverer struct {
	refs []githubactions.ActionRef
	err  error
}

func (f fixedDiscoverer) Discover() ([]githubactions.ActionRef, error) { return f.refs, f.err }

func TestCacheWalkDiscoverer(t *testing.T) {
	t.Run("verify when the cache holds actions then each is returned once", func(t *testing.T) {
		_, cacheDir := runnerSpec{cacheDirs: []string{"actions/checkout/v4", "actions/setup-go/v5"}}.build(t)
		refs, err := cacheWalkDiscoverer{actionsCacheDir: cacheDir}.Discover()
		require.NoError(t, err)
		var got []string
		for _, r := range refs {
			got = append(got, r.Owner+"/"+r.Repo+"@"+r.Ref)
		}
		assert.ElementsMatch(t, []string{"actions/checkout@v4", "actions/setup-go@v5"}, got)
	})
	t.Run("verify when the cache is empty then it reports the cache as unreadable", func(t *testing.T) {
		_, cacheDir := runnerSpec{}.build(t)
		_, err := cacheWalkDiscoverer{actionsCacheDir: cacheDir}.Discover()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot read the GitHub Actions cache")
	})
	t.Run("verify when an entry cannot be accounted for then it fails", func(t *testing.T) {
		_, cacheDir := runnerSpec{}.build(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cacheDir, "actions", "checkout", "v4"), 0o755))
		_, err := cacheWalkDiscoverer{actionsCacheDir: cacheDir}.Discover()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot account for every entry")
	})
}

func TestCurationActionsCommand_Run_UsesTheConfiguredDiscoverer(t *testing.T) {
	pinRunnerEnv(t, "octo/repo", "", "")
	_, cacheDir := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}.build(t)
	cmd := NewCurationActionsCommand().
		SetActionsCacheDir(cacheDir).
		SetDecider(&scriptedDecider{}).
		SetActionDiscoverer(fixedDiscoverer{refs: []githubactions.ActionRef{
			{Owner: "only", Repo: "this", Ref: "v9", Path: filepath.Join(cacheDir, "actions", "checkout", "v4")},
		}})
	report, err := captureReport(t, cmd)
	require.NoError(t, err)
	assert.Contains(t, report, "only/this")
	assert.NotContains(t, report, "actions/checkout")
}

func TestCurationActionsCommand_Run_ReturnsTheDiscovererError(t *testing.T) {
	pinRunnerEnv(t, "octo/repo", "", "")
	want := errors.New("discovery failed")
	cmd := NewCurationActionsCommand().
		SetActionsCacheDir(t.TempDir()).
		SetDecider(&scriptedDecider{}).
		SetActionDiscoverer(fixedDiscoverer{err: want})
	var buf bytes.Buffer
	previous := log.Logger
	log.SetLogger(log.NewLogger(log.INFO, &buf))
	t.Cleanup(func() { log.SetLogger(previous) })
	assert.ErrorIs(t, cmd.Run(), want)
}

const (
	testShaV4 = "11d5960a326750d5838078e36cf38b85af677262"
	testShaV3 = "a37ce9120846195fa4ece8f58b268e6043cb2f26"
)

// hookRunner lays out a runner directory: _diag files, and _work/_actions entries with watermarks.
// It sets this job's run identity and, as the runner does, writes the job message naming that run
// right after each Worker start line.
func hookRunner(t *testing.T, diag map[string]string, cacheDirs []string) (runnerDir, cacheDir string) {
	t.Helper()
	return hookRunnerForJob(t, "", diag, cacheDirs)
}

// hookRunnerForJob is hookRunner with job appended to the job message's top-level fields; jobFields
// builds one the trust assessment can read.
func hookRunnerForJob(t *testing.T, job string, diag map[string]string, cacheDirs []string) (runnerDir, cacheDir string) {
	t.Helper()
	t.Setenv(githubactions.RunIDEnvVar, "37295940958")
	t.Setenv(githubactions.RunAttemptEnvVar, "1")
	if os.Getenv(githubactions.GithubRepoEnvVar) == "" {
		t.Setenv(githubactions.GithubRepoEnvVar, "octo/repo")
	}
	run := githubactions.RunIdentityFromEnv()
	jobMessage := "[x INFO Worker] Job message:\n {\"contextData\": {\"github\": {\"t\": 2, \"d\": [" +
		"{\"k\": \"repository\", \"v\": \"" + run.Repository + "\"}, {\"k\": \"run_id\", \"v\": \"" + run.RunID + "\"}, " +
		"{\"k\": \"run_attempt\", \"v\": \"" + run.RunAttempt + "\"}]}}" + job + "}\n"
	runnerDir = t.TempDir()
	for name, content := range diag {
		path := filepath.Join(runnerDir, "_diag", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		content = strings.ReplaceAll(content, "Worker] Version: 2.337.0\n", "Worker] Version: 2.337.0\n"+jobMessage)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	cacheDir = filepath.Join(runnerDir, "_work", "_actions")
	for _, dir := range cacheDirs {
		path := filepath.Join(cacheDir, filepath.FromSlash(dir))
		require.NoError(t, os.MkdirAll(path, 0o755))
		require.NoError(t, os.WriteFile(path+".completed", nil, 0o644))
	}
	return runnerDir, cacheDir
}

func TestRunnerLogDiscoverer(t *testing.T) {
	start := "[x INFO Worker] Version: 2.337.0\n"
	save := func(repo, sha string) string {
		return workerLine("Save archive 'https://codeload.github.com/actions/"+repo+"/tar.gz/"+sha+"' into x") +
			"Request URL: https://codeload.github.com/actions/" + repo + "/tar.gz/" + sha + " X-GitHub-Request-Id: A\n"
	}
	t.Run("verify when no cache is configured then every action is downloaded with its SHA", func(t *testing.T) {
		runnerDir, cacheDir := hookRunner(t, map[string]string{
			"pages/a_1.log":                  "Download action repository 'actions/checkout@v4' (SHA:" + testShaV4 + ")\n",
			"Worker_20261005-050652-utc.log": start + save("checkout", testShaV4),
		}, []string{"actions/checkout/v4"})
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
		require.NoError(t, err)
		require.Len(t, refs, 1)
		assert.Equal(t, testShaV4, refs[0].RunnerSHA)
	})
	t.Run("verify when an earlier job planted a Worker log that sorts last then the runner's SHA still decides", func(t *testing.T) {
		runnerDir, cacheDir := hookRunner(t, map[string]string{
			"pages/a_1.log":                  "Download action repository 'actions/checkout@v4' (SHA:" + testShaV4 + ")\n",
			"Worker_20261005-050652-utc.log": start + save("checkout", testShaV4),
			"Worker_29991231-235959-utc.log": start + save("checkout", testShaV3),
		}, []string{"actions/checkout/v4"})
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
		require.NoError(t, err)
		require.Len(t, refs, 1)
		assert.Equal(t, testShaV4, refs[0].RunnerSHA)
	})
	t.Run("verify when the logs name nothing then it falls back to the cache walk", func(t *testing.T) {
		runnerDir, cacheDir := hookRunner(t, map[string]string{"Worker_20261005-050652-utc.log": start},
			[]string{"actions/checkout/v4"})
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
		require.NoError(t, err)
		require.Len(t, refs, 1)
		assert.Empty(t, refs[0].RunnerSHA)
	})
	t.Run("verify when the diag folder is unreadable then it falls back to the cache walk", func(t *testing.T) {
		runnerDir, cacheDir := hookRunner(t, nil, []string{"actions/checkout/v4"})
		var refs []githubactions.ActionRef
		out := captureLog(t, log.INFO, func() {
			var err error
			refs, err = runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
			require.NoError(t, err)
		})
		require.Len(t, refs, 1)
		assert.Equal(t, githubactions.ReasonLogUntrusted, refs[0].ContentReason)
		assert.Equal(t, 1, strings.Count(out, "[Warn]"), "one warning carries the error: %s", out)
	})
	t.Run("verify when the runner fetched an action that is not in the cache then it fails", func(t *testing.T) {
		runnerDir, cacheDir := hookRunner(t, map[string]string{
			"Worker_20261005-050652-utc.log": start + save("checkout", testShaV4) + save("setup-node", testShaV3),
		}, []string{"actions/checkout/v4"})
		_, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "actions/setup-node@"+testShaV3)
	})
	t.Run("verify when a ref cannot be recovered then it is still curated without a runner SHA", func(t *testing.T) {
		runnerDir, cacheDir := hookRunner(t, map[string]string{
			"Worker_20261005-050652-utc.log": start + save("checkout", testShaV4) + save("checkout", testShaV3),
		}, []string{"actions/checkout/v4", "actions/checkout/v3"})
		same := time.Now()
		for _, ref := range []string{"v4", "v3"} {
			require.NoError(t, os.Chtimes(filepath.Join(cacheDir, "actions", "checkout", ref+".completed"), same, same))
		}
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
		require.NoError(t, err)
		assert.Equal(t, map[string]refFacts{
			"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA},
			"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA},
			"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
			"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
		}, factsOf(refs), "each logged commit is still decided, by its SHA")
	})
}

func TestCurationActionsCommand_Run_HookModeReportHasTheStepModeColumns(t *testing.T) {
	pinRunnerEnv(t, "octo/repo", "", "")
	runnerDir, cacheDir := hookRunner(t, map[string]string{
		"pages/a_1.log":                  "Download action repository 'actions/checkout@v4' (SHA:" + testShaV4 + ")\n",
		"Worker_20261005-050652-utc.log": "[x INFO Worker] Version: 2.337.0\n" + workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/"+testShaV4+"' into x"),
	}, []string{"actions/checkout/v4"})
	cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "").SetDecider(&scriptedDecider{})
	report, err := captureReport(t, cmd)
	require.NoError(t, err)
	assert.Contains(t, report, "| Action | Ref | Status | Notes |")
	assert.NotContains(t, report, "Runner SHA")
}

func TestRunnerLogDiscovererAcceptsARenamedRepository(t *testing.T) {
	runnerDir, cacheDir := hookRunner(t, map[string]string{
		"pages/a_1.log":                  "Download action repository 'oldowner/tool@v1' (SHA:" + testShaV4 + ")\n",
		"Worker_20261005-050652-utc.log": "[x INFO Worker] Version: 2.337.0\n" + workerLine("Save archive 'https://api.github.com/repos/newowner/tool/tarball/"+testShaV4+"' into x"),
	}, []string{"oldowner/tool/v1"})
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"oldowner/tool@v1":           {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
		"newowner/tool#" + testShaV4: {SHA: testShaV4, BySHA: true},
	}, factsOf(refs), "the folder is compared, and the commit is decided under the name GitHub resolved")
}

func TestCurationActionsCommand_Run_HookModeDoesNotAttributeFromTheWorkspace(t *testing.T) {
	// At job start a persistent runner's workspace still holds the previous run's checkout, so a
	// workflow file found there may not be this job's. Hook mode must not attribute from it, nor
	// drop the caveat about local composite actions it cannot see.
	pinRunnerEnv(t, testGithubRepo, "", "")
	spec := runnerSpec{
		cacheDirs:    []string{"actions/checkout/v4"},
		workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n",
	}
	runnerDir, _ := hookRunner(t, map[string]string{
		"Worker_20261005-050652-utc.log": "[x INFO Worker] Version: 2.337.0\n" + workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/"+testShaV4+"' into x"),
	}, nil)
	cmd := spec.newCommand(t, writtenWorkflowFile, "build", &scriptedDecider{}).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "")
	report, err := captureReport(t, cmd)
	require.NoError(t, err)
	assert.NotContains(t, report, "| Parent |")
	assert.Contains(t, report, "Local composite actions (uses: ./...) are not curated")
}

func TestRunnerLogDiscovererSetupLineCoverage(t *testing.T) {
	start := "[x INFO Worker] Version: 2.337.0\n"
	save := func(sha string) string {
		return workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + sha + "' into x")
	}
	setup := func(ref, sha string) string {
		return "Download action repository 'actions/checkout@" + ref + "' (SHA:" + sha + ")\n"
	}
	t.Run("verify when the setup lines cover every fetched action of a trusted job then the cache is not walked", func(t *testing.T) {
		runnerDir, _ := hookRunnerForJob(t, jobFields("actions/checkout"), map[string]string{
			"pages/a_1.log":                  setup("v4", testShaV4) + setup("v3", testShaV3),
			"Worker_20261005-050652-utc.log": start + save(testShaV4) + save(testShaV3),
		}, nil)
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: filepath.Join(runnerDir, "no-such-cache"),
			mode: githubactions.ModeHook}.Discover()
		require.NoError(t, err)
		got := map[string]string{}
		for _, r := range refs {
			got[r.Owner+"/"+r.Repo+"@"+r.Ref] = r.RunnerSHA
		}
		assert.Equal(t, map[string]string{
			"actions/checkout@v4": testShaV4,
			"actions/checkout@v3": testShaV3,
		}, got)
	})
	t.Run("verify when the setup lines cover only some fetched actions then the cache walk recovers the rest", func(t *testing.T) {
		runnerDir, cacheDir := hookRunner(t, map[string]string{
			"blocks/b_1.log":                 setup("v4", testShaV4),
			"Worker_20261005-050652-utc.log": start + save(testShaV4) + save(testShaV3),
		}, []string{"actions/checkout/v4", "actions/checkout/v3"})
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
		require.NoError(t, err)
		got := map[string]string{}
		for _, r := range refs {
			got[r.Ref] = r.RunnerSHA
		}
		assert.Equal(t, map[string]string{"v4": testShaV4, "v3": testShaV3}, got)
	})
}

func TestCurationActionsCommand_Run_StepSummary(t *testing.T) {
	logs := map[string]string{
		"pages/a_1.log":                  "Download action repository 'actions/checkout@v4' (SHA:" + testShaV4 + ")\n",
		"Worker_20261005-050652-utc.log": "[x INFO Worker] Version: 2.337.0\n" + workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/"+testShaV4+"' into x"),
	}
	// stepSummary is the file the runner hands the hook, already holding what an earlier hook wrote.
	stepSummary := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "step_summary.md")
		require.NoError(t, os.WriteFile(path, []byte("earlier hook\n"), 0o644))
		t.Setenv(stepSummaryEnvVar, path)
		return path
	}
	t.Run("verify when run as the hook then the report is appended to the runner's step summary", func(t *testing.T) {
		pinRunnerEnv(t, "octo/repo", "", "")
		path := stepSummary(t)
		runnerDir, cacheDir := hookRunner(t, logs, []string{"actions/checkout/v4"})
		cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "").SetDecider(&scriptedDecider{})
		_, err := captureReport(t, cmd)
		require.NoError(t, err)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(content), "earlier hook\n"), "the summary must be appended, not replace what is there")
		assert.Contains(t, string(content), "GitHub Actions Curation")
		assert.Contains(t, string(content), "| actions/checkout | v4 |")
	})
	t.Run("verify when run as a step then the step summary is left to setup-jfrog-cli", func(t *testing.T) {
		pinRunnerEnv(t, "octo/repo", "", "")
		path := stepSummary(t)
		_, cacheDir := hookRunner(t, logs, []string{"actions/checkout/v4"})
		cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(&scriptedDecider{})
		_, err := captureReport(t, cmd)
		require.NoError(t, err)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "earlier hook\n", string(content))
	})
}

// workerLine returns message as the runner writes it into the Worker log.
func workerLine(message string) string {
	return "[2026-10-07 11:03:03Z INFO ActionManager] " + message + "\n"
}

const (
	testShaNode = "1d0ff469b7ec7b3cb9d8673fde0c81c44821de2a"
	testShaEvil = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	testShaSelf = "5e1f5e1f5e1f5e1f5e1f5e1f5e1f5e1f5e1f5e1f"
)

// refFacts is what the trust assessment decides for one discovered ref.
type refFacts struct {
	SHA    string   // RunnerSHA
	Reason string   // ContentReason
	BySHA  bool     // Verification is VerifyLoggedSHA
	Logged []string // LoggedSHAs
}

// factsOf keys each ref as owner/repo@ref, or owner/repo#sha for an unpaired logged commit, which
// has no ref and no path.
func factsOf(refs []githubactions.ActionRef) map[string]refFacts {
	got := map[string]refFacts{}
	for _, r := range refs {
		key := r.Owner + "/" + r.Repo + "@" + r.Ref
		if r.Ref == "" && r.Path == "" {
			key = r.Owner + "/" + r.Repo + "#" + r.RunnerSHA
		}
		got[key] = refFacts{SHA: r.RunnerSHA, Reason: r.ContentReason, BySHA: r.Verification == githubactions.VerifyLoggedSHA, Logged: r.LoggedSHAs}
	}
	return got
}

// captureLog runs fn with the logger at level writing to a buffer, and returns what it wrote.
func captureLog(t *testing.T, level log.LevelType, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Logger
	log.SetLogger(log.NewLogger(level, &buf))
	defer log.SetLogger(previous)
	fn()
	return buf.String()
}

// jobFields is the job message's timeline (id "a", which the setup buffer files below are named
// for) and one GitHub repository step per "owner/repo" in steps, in order.
func jobFields(steps ...string) string {
	var sb strings.Builder
	sb.WriteString(`, "timeline": {"id": "a"}, "steps": [`)
	for i, s := range steps {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(`{"reference": {"type": "repository", "repositoryType": "GitHub", "name": ` + strconv.Quote(s) + `, "ref": "v1"}}`)
	}
	sb.WriteString("]")
	return sb.String()
}

func savedLine(owner, repo, sha string) string {
	return workerLine("Save archive 'https://codeload.github.com/" + owner + "/" + repo + "/tar.gz/" + sha + "' into x")
}

func cachedLine(owner, repo, sha string) string {
	return workerLine("Check if action archive '" + owner + "/" + repo + "@" + sha + "' exists in cache directory '/cache'")
}

func setupLine(owner, repo, ref, sha string) string {
	return "Download action repository '" + owner + "/" + repo + "@" + ref + "' (SHA:" + sha + ")\n"
}

const workerStart = "[x INFO Worker] Version: 2.337.0\n"

// trustedHookJob is a job a hook can trust: checkout and setup-node, each logged by the Worker and
// named by a setup line.
func trustedHookJob(t *testing.T) (runnerDir, cacheDir string) {
	t.Helper()
	return hookRunnerForJob(t, jobFields("actions/checkout", "actions/setup-node"), map[string]string{
		"pages/a_1.log": setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "setup-node", "v4", testShaNode),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) +
			savedLine("actions", "setup-node", testShaNode),
	}, []string{"actions/checkout/v4", "actions/setup-node/v4"})
}

func TestRunnerLogDiscovererApprovesByLoggedSHAWhenTrusted(t *testing.T) {
	runnerDir, cacheDir := trustedHookJob(t)
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4":   {SHA: testShaV4, BySHA: true},
		"actions/setup-node@v4": {SHA: testShaNode, BySHA: true},
	}, factsOf(refs))
}

func TestRunnerLogDiscovererUnsetModeNeverUsesTheFastPath(t *testing.T) {
	runnerDir, cacheDir := trustedHookJob(t)
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4":   {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
		"actions/setup-node@v4": {SHA: testShaNode, Reason: githubactions.ReasonLogUntrusted},
	}, factsOf(refs))
}

func TestRunnerLogDiscovererStaleSetupLineDowngradesThatAction(t *testing.T) {
	worker := workerStart + savedLine("actions", "checkout", testShaV4) + savedLine("actions", "setup-node", testShaNode)
	tests := []struct {
		name  string
		lines string
		want  map[string]refFacts
	}{
		{
			name:  "verify when a setup line of the job names a SHA the Worker never fetched then only that action is compared",
			lines: setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "setup-node", "v4", testShaEvil),
			want: map[string]refFacts{
				"actions/checkout@v4":   {SHA: testShaV4, BySHA: true},
				"actions/setup-node@v4": {SHA: testShaNode, Reason: githubactions.ReasonStaleLine},
			},
		},
		{
			name: "verify when the stale line is for a repository outside the job then nothing changes",
			lines: setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "setup-node", "v4", testShaNode) +
				setupLine("attackerpre", "evil", "v9", testShaEvil),
			want: map[string]refFacts{
				"actions/checkout@v4":   {SHA: testShaV4, BySHA: true},
				"actions/setup-node@v4": {SHA: testShaNode, BySHA: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerDir, cacheDir := hookRunnerForJob(t, jobFields("actions/checkout", "actions/setup-node"), map[string]string{
				"pages/a_1.log":                  tt.lines,
				"Worker_20261005-050652-utc.log": worker,
			}, []string{"actions/checkout/v4", "actions/setup-node/v4"})
			refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
			require.NoError(t, err)
			assert.Equal(t, tt.want, factsOf(refs))
		})
	}
}

func TestRunnerLogDiscovererAttackerFirstDowngradesInPreMode(t *testing.T) {
	diag := map[string]string{
		"pages/a_1.log": setupLine("attacker", "first", "v1", testShaEvil) + setupLine("jfrog", "curate", "v1", testShaSelf),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("attacker", "first", testShaEvil) +
			savedLine("jfrog", "curate", testShaSelf),
	}
	cacheDirs := []string{"attacker/first/v1", "jfrog/curate/v1"}
	tests := []struct {
		name  string
		steps []string
		want  map[string]refFacts
	}{
		{
			name:  "verify when another action runs before this one then the job is compared by content",
			steps: []string{"attacker/first", "jfrog/curate"},
			want: map[string]refFacts{
				"attacker/first@v1": {SHA: testShaEvil, Reason: githubactions.ReasonLogUntrusted},
				"jfrog/curate@v1":   {SHA: testShaSelf, Reason: githubactions.ReasonLogUntrusted},
			},
		},
		{
			name:  "verify when this action is the first step then the logged SHAs decide",
			steps: []string{"jfrog/curate", "attacker/first"},
			want: map[string]refFacts{
				"attacker/first@v1": {SHA: testShaEvil, BySHA: true},
				"jfrog/curate@v1":   {SHA: testShaSelf, BySHA: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerDir, cacheDir := hookRunnerForJob(t, jobFields(tt.steps...), diag, cacheDirs)
			refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModePre, self: "jfrog/curate"}.Discover()
			require.NoError(t, err)
			assert.Equal(t, tt.want, factsOf(refs))
		})
	}
}

func TestRunnerLogDiscovererLoggedSHAsOfARefWithoutARunnerSHA(t *testing.T) {
	// Two checkout folders of one age and no setup line: nothing pairs either with a logged commit,
	// so each carries the repository's logged commits for the decider to pair through the refs list.
	diag := func(extra map[string]string) map[string]string {
		diag := map[string]string{
			"pages/a_1.log": setupLine("actions", "setup-node", "v4", testShaNode),
			"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", strings.ToUpper(testShaV4)) +
				savedLine("actions", "checkout", testShaV3) + savedLine("actions", "setup-node", testShaNode),
		}
		for k, v := range extra {
			diag[k] = v
		}
		return diag
	}
	cacheDirs := []string{"actions/checkout/v4", "actions/checkout/v3", "actions/setup-node/v4"}
	tests := []struct {
		name  string
		extra map[string]string
		want  map[string]refFacts
	}{
		{
			name: "verify when the job is trusted then only the unpaired refs carry their repository's logged commits",
			want: map[string]refFacts{
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
				"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
				"actions/setup-node@v4":         {SHA: testShaNode, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
		{
			name:  "verify when the job is untrusted then no ref carries logged commits",
			extra: map[string]string{plantedWorkerLog: "[x INFO Worker] continued\n"},
			want: map[string]refFacts{
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA},
				"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA},
				"actions/setup-node@v4":         {SHA: testShaNode, Reason: githubactions.ReasonLogUntrusted},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerDir, cacheDir := hookRunnerForJob(t, jobFields("actions/checkout", "actions/setup-node"), diag(tt.extra), cacheDirs)
			same := time.Now()
			for _, ref := range []string{"v4", "v3"} {
				require.NoError(t, os.Chtimes(filepath.Join(cacheDir, "actions", "checkout", ref+".completed"), same, same))
			}
			refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
			require.NoError(t, err)
			assert.Equal(t, tt.want, factsOf(refs))
		})
	}
}

func TestRunnerLogDiscovererAmbiguousPairLeavesOnlyThatActionOnContent(t *testing.T) {
	runnerDir, cacheDir := hookRunnerForJob(t, jobFields("actions/checkout", "actions/setup-node"), map[string]string{
		"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) +
			savedLine("actions", "checkout", testShaV3) + savedLine("actions", "setup-node", testShaNode),
	}, []string{"actions/checkout/v4", "actions/checkout/v3", "actions/setup-node/v4"})
	same := time.Now()
	for _, ref := range []string{"v4", "v3"} {
		require.NoError(t, os.Chtimes(filepath.Join(cacheDir, "actions", "checkout", ref+".completed"), same, same))
	}
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
		"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
		"actions/setup-node@v4":         {SHA: testShaNode, BySHA: true},
		"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
		"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
	}, factsOf(refs))
}

// swappedSHAJob is a job whose aa/aa folder holds the commit the Worker logged as testShaEvil, while
// a forged setup line hands it testShaV4, which the Worker logged for actions/checkout. aa/aa sorts
// first, so the forged line is read before checkout's own.
func swappedSHAJob(t *testing.T) (runnerDir, cacheDir string) {
	t.Helper()
	return hookRunnerForJob(t, jobFields("aa/aa", "actions/checkout"), map[string]string{
		"pages/a_1.log": setupLine("aa", "aa", "v1", testShaV4) + setupLine("actions", "checkout", "v4", testShaV4),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) +
			savedLine("aa", "aa", testShaEvil),
	}, []string{"aa/aa/v1", "actions/checkout/v4"})
}

func TestRunnerLogDiscovererSwappedSHAIsStillDecided(t *testing.T) {
	runnerDir, cacheDir := swappedSHAJob(t)
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"aa/aa@v1":             {SHA: testShaV4, Reason: githubactions.ReasonRenamed},
		"actions/checkout@v4":  {SHA: testShaV4, BySHA: true},
		"aa/aa#" + testShaEvil: {SHA: testShaEvil, BySHA: true},
	}, factsOf(refs))
}

// plantedWorkerLog is a Worker log newer than the job's own and without a start line, which a job
// could have written.
const plantedWorkerLog = "Worker_20261005-060000-utc.log"

func TestRunnerLogDiscovererUnpairedLoggedSHAIsDecidedBySHAEvenWhenUntrusted(t *testing.T) {
	runnerDir, cacheDir := hookRunnerForJob(t, jobFields("actions/checkout"), map[string]string{
		"pages/a_1.log": setupLine("actions", "checkout", "v4", testShaV4),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) +
			savedLine("actions", "checkout", testShaV3),
		plantedWorkerLog: "[x INFO Worker] continued\n",
	}, []string{"actions/checkout/v4"})
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4":           {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
		"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
	}, factsOf(refs))
}

func TestRunnerLogDiscovererUnreadableJobMessageInHookDowngrades(t *testing.T) {
	runnerDir, cacheDir := hookRunnerForJob(t, `, "timeline": {"id": "a"}, "steps": 5`, map[string]string{
		"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4),
	}, []string{"actions/checkout/v4"})
	var refs []githubactions.ActionRef
	out := captureLog(t, log.INFO, func() {
		var err error
		refs, err = runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
		require.NoError(t, err)
	})
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4": {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
	}, factsOf(refs))
	assert.Equal(t, 1, strings.Count(out, "cannot be trusted"), "one warning per untrusted job: %s", out)
	assert.Contains(t, out, "the job message could not be read")
}

func TestRunnerLogDiscovererPlantedWorkerFileDowngrades(t *testing.T) {
	runnerDir, cacheDir := trustedHookJob(t)
	require.NoError(t, os.WriteFile(filepath.Join(runnerDir, "_diag", plantedWorkerLog), []byte("[x INFO Worker] continued\n"), 0o644))
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4":   {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
		"actions/setup-node@v4": {SHA: testShaNode, Reason: githubactions.ReasonLogUntrusted},
	}, factsOf(refs))
}

func TestRunnerLogDiscovererCacheSourceOnSelfHostedDowngradesThatActionOnly(t *testing.T) {
	runnerDir, cacheDir := hookRunnerForJob(t, jobFields("actions/checkout", "actions/setup-node"), map[string]string{
		"pages/a_1.log": setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "setup-node", "v4", testShaNode),
		"Worker_20261005-050652-utc.log": workerStart + cachedLine("actions", "checkout", testShaV4) +
			savedLine("actions", "setup-node", testShaNode),
	}, []string{"actions/checkout/v4", "actions/setup-node/v4"})
	var refs []githubactions.ActionRef
	out := captureLog(t, log.DEBUG, func() {
		var err error
		refs, err = runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
		require.NoError(t, err)
	})
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4":   {SHA: testShaV4, Reason: githubactions.ReasonCacheSource},
		"actions/setup-node@v4": {SHA: testShaNode, BySHA: true},
	}, factsOf(refs))
	assert.NotContains(t, out, "cannot be trusted", "an evidence rule downgrades the action, not the job")
	assert.Contains(t, out, `"actions/checkout"`)
	assert.Contains(t, out, githubactions.ReasonCacheSource)
}

// checkoutArchiveCache is an archive cache holding actions/checkout at testShaV4, made read-only by
// its owner, the user running the test, when readOnly is set. Write permission is restored on cleanup so the temp dir can be removed.
func checkoutArchiveCache(t *testing.T, readOnly bool) string {
	t.Helper()
	root := t.TempDir()
	folder := filepath.Join(root, "actions_checkout")
	require.NoError(t, os.MkdirAll(folder, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(folder, testShaV4+".tar.gz"), []byte("x"), 0o644))
	if readOnly {
		t.Cleanup(func() {
			for _, dir := range []string{root, folder} {
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Logf("restoring write permission on %s: %v", dir, err)
				}
			}
		})
		require.NoError(t, os.Chmod(filepath.Join(folder, testShaV4+".tar.gz"), 0o444))
		require.NoError(t, os.Chmod(folder, 0o555))
		require.NoError(t, os.Chmod(root, 0o555))
	}
	return root
}

func TestRunnerLogDiscovererCacheSourceTrust(t *testing.T) {
	tests := []struct {
		name             string
		readOnlyCache    bool
		trustActionCache bool
		wantCheckout     refFacts
	}{
		{
			name:         "verify when the cache the action came from is writable then that action is verified by content",
			wantCheckout: refFacts{SHA: testShaV4, Reason: githubactions.ReasonCacheSource},
		},
		{
			name:          "verify when the job's user owns a cache it made read-only then that action is still verified by content",
			readOnlyCache: true,
			wantCheckout:  refFacts{SHA: testShaV4, Reason: githubactions.ReasonCacheSource},
		},
		{
			name:             "verify when the admin trusts the action cache then a writable cache is decided by its logged SHA",
			trustActionCache: true,
			wantCheckout:     refFacts{SHA: testShaV4, BySHA: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archives := checkoutArchiveCache(t, tt.readOnlyCache)
			runnerDir, cacheDir := hookRunnerForJob(t, jobFields("actions/checkout", "actions/setup-node"), map[string]string{
				"pages/a_1.log": setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "setup-node", "v4", testShaNode),
				"Worker_20261005-050652-utc.log": workerStart +
					workerLine("Check if action archive 'actions/checkout@"+testShaV4+"' already exists in cache directory '"+archives+"'") +
					workerLine("Found action archive '"+filepath.Join(archives, "actions_checkout", testShaV4+".tar.gz")+"' in cache directory '"+archives+"'") +
					savedLine("actions", "setup-node", testShaNode),
			}, []string{"actions/checkout/v4", "actions/setup-node/v4"})
			refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook,
				trustActionCache: tt.trustActionCache}.Discover()
			require.NoError(t, err)
			assert.Equal(t, map[string]refFacts{
				"actions/checkout@v4":   tt.wantCheckout,
				"actions/setup-node@v4": {SHA: testShaNode, BySHA: true},
			}, factsOf(refs))
		})
	}
}

func TestAssessmentReasonsAreSanitised(t *testing.T) {
	// The job's first step is named to start a workflow command, and so is the action's own name,
	// which the job-level reason prints.
	runnerDir, cacheDir := hookRunnerForJob(t, jobFields("evil\n::warning::steptext", "jfrog/curate"), map[string]string{
		"pages/a_1.log":                  setupLine("jfrog", "curate", "v1", testShaSelf),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("jfrog", "curate", testShaSelf),
	}, []string{"jfrog/curate/v1"})
	var refs []githubactions.ActionRef
	out := captureLog(t, log.DEBUG, func() {
		var err error
		refs, err = runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModePre,
			self: "jfrog/curate\n::error::boom"}.Discover()
		require.NoError(t, err)
	})
	assert.Contains(t, out, "cannot be trusted")
	assert.Contains(t, out, "boom", "the action's name is printed in the reason, so the test can catch an unsanitised one")
	assert.NotContains(t, out, "steptext", "a step name is never printed")
	for _, line := range strings.Split(out, "\n") {
		assert.False(t, strings.HasPrefix(strings.TrimSpace(line), "::"), "a log line starts a workflow command: %q", line)
	}
	for _, r := range refs {
		assert.Equal(t, githubactions.ReasonLogUntrusted, r.ContentReason)
	}
}

func TestRunnerLogDiscovererDecidesEveryLoggedCommitUnderItsOwnName(t *testing.T) {
	// A forged line hands aa/aa checkout's testShaV4. Checkout logged two commits, so no other rule
	// gives testShaV4 back to a checkout folder; it must still be decided as checkout's.
	worker := workerStart + savedLine("actions", "checkout", testShaV4) + savedLine("actions", "checkout", testShaV3)
	tests := []struct {
		name      string
		lines     string
		worker    string
		cacheDirs []string
		want      map[string]refFacts
	}{
		{
			name: "verify when the cache is walked then the stolen commit is decided under its own name",
			lines: setupLine("aa", "aa", "v1", testShaV4) + setupLine("actions", "checkout", "v4", testShaV4) +
				setupLine("actions", "checkout", "v3", testShaV3),
			worker:    worker + savedLine("aa", "aa", testShaEvil),
			cacheDirs: []string{"aa/aa/v1", "actions/checkout/v4", "actions/checkout/v3"},
			want: map[string]refFacts{
				"aa/aa@v1":                      {SHA: testShaV4, Reason: githubactions.ReasonRenamed},
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
				"actions/checkout@v3":           {SHA: testShaV3, BySHA: true},
				"aa/aa#" + testShaEvil:          {SHA: testShaEvil, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
			},
		},
		{
			name:      "verify when the setup lines account for the job then the stolen commit is decided under its own name",
			lines:     setupLine("aa", "aa", "v1", testShaV4) + setupLine("actions", "checkout", "v3", testShaV3),
			worker:    worker,
			cacheDirs: []string{"aa/aa/v1", "actions/checkout/v3"},
			want: map[string]refFacts{
				"aa/aa@v1":                      {SHA: testShaV4, Reason: githubactions.ReasonRenamed},
				"actions/checkout@v3":           {SHA: testShaV3, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerDir, cacheDir := hookRunnerForJob(t, jobFields("aa/aa", "actions/checkout"), map[string]string{
				"pages/a_1.log":                  tt.lines,
				"Worker_20261005-050652-utc.log": tt.worker,
			}, tt.cacheDirs)
			refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: githubactions.ModeHook}.Discover()
			require.NoError(t, err)
			assert.Equal(t, tt.want, factsOf(refs))
		})
	}
}

func TestCurationActionsCommand_Run_HookModeForgedSetupLineCannotApproveAnEvilCopy(t *testing.T) {
	// The real decider: the forged line hands aa/aa's evil copy checkout's approved SHA, so the copy
	// is compared at that SHA and fails, and the commit the Worker really fetched for aa/aa is decided too.
	const vcsRepo = "github-vcs"
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(stepSummaryEnvVar, "")
	runnerDir, cacheDir := swappedSHAJob(t)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "aa", "aa", "v1", "action.yml"), []byte("name: evil\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "actions", "checkout", "v4", "action.yml"), []byte("name: checkout\n"), 0o644))
	approved := actionTarGz(t, map[string]string{"action.yml": "name: checkout\n"})
	served := map[string][]byte{
		"downloadCommit/" + vcsRepo + "/actions/checkout/" + testShaV4: approved,
		"downloadCommit/" + vcsRepo + "/aa/aa/" + testShaV4:            approved,
		"downloadCommit/" + vcsRepo + "/aa/aa/" + testShaEvil:          actionTarGz(t, map[string]string{"action.yml": "name: evil\n"}),
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
		if _, err := w.Write(archive); err != nil {
			t.Errorf("serving %s: %v", key, err)
		}
	}))
	t.Cleanup(server.Close)
	decider, err := githubactions.NewArtifactoryActionCurationDecider(&config.ServerDetails{ArtifactoryUrl: server.URL + "/artifactory/", AccessToken: "test-token"})
	require.NoError(t, err)
	cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "").
		SetVcsRepoResolver(&fixedResolver{repo: vcsRepo}).SetDecider(decider)

	report, err := captureReport(t, cmd)

	assert.ElementsMatch(t, slices.Collect(maps.Keys(served)), requested, "every logged commit and every folder must be decided")
	assert.Contains(t, report, "| aa/aa | v1 | Rejected | not able to decide since content is mismatched (action.yml differs)")
	assert.Contains(t, report, "verified by content: "+githubactions.ReasonRenamed)
	assert.Contains(t, report, "| actions/checkout | v4 | Approved | resolved SHA: "+testShaV4+" |")
	assert.Contains(t, report, "| aa/aa | "+testShaEvil+" | Approved | resolved SHA: "+testShaEvil+"; unpaired logged commit |")
	require.Error(t, err, "the evil copy must fail the gate")
	assert.ErrorContains(t, err, "aa/aa@v1")
}

// refRecorder approves every action and records the refs it was asked to decide, so a test sees how
// each one was to be verified.
type refRecorder struct {
	mu   sync.Mutex
	refs []githubactions.ActionRef
}

func (r *refRecorder) Decide(_ context.Context, _ string, ref githubactions.ActionRef) (githubactions.ActionCurationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	return githubactions.ActionCurationResult{Status: githubactions.ActionApproved}, nil
}

func TestCurationActionsCommand_Run_FromPreWithAnInvisibleRunnerComparesEveryActionByContent(t *testing.T) {
	// A container job: the Worker is not in the process tree, so the pre has only the action cache.
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
	t.Setenv(githubactions.RunnerEnvironmentEnvVar, "github-hosted")
	summary := filepath.Join(t.TempDir(), "step_summary.md")
	t.Setenv(stepSummaryEnvVar, summary)
	spec := runnerSpec{
		cacheDirs:    []string{"actions/checkout/v4", "jfrog/curate/v1"},
		workflowYAML: "jobs:\n  build:\n    steps:\n      - uses: jfrog/curate@v1\n      - uses: actions/checkout@v4\n",
	}
	decider := &refRecorder{}
	cmd := spec.newCommand(t, writtenWorkflowFile, "build", decider).SetFromPre(true)
	cmd.runnerDirFinder = func() (string, error) { return "", githubactions.ErrRunnerNotVisible }

	report, err := captureReport(t, cmd)

	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"actions/checkout@v4": {Reason: githubactions.ReasonNoSHA},
		"jfrog/curate@v1":     {Reason: githubactions.ReasonNoSHA},
	}, factsOf(decider.refs))
	assert.Contains(t, report, "not visible")
	assert.NotContains(t, report, "| Parent |", "a pre runs before any checkout, so the workspace's workflow file is not this job's")
	content, err := os.ReadFile(summary)
	require.NoError(t, err)
	assert.Contains(t, string(content), "| actions/checkout | v4 |")
}

func TestCurationActionsCommand_Run_FromPreAsTheFirstStepDecidesByLoggedSHA(t *testing.T) {
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
	t.Setenv(githubactions.RunnerEnvironmentEnvVar, "github-hosted")
	t.Setenv(stepSummaryEnvVar, "")
	// checkout came from the runner's action cache, which on a GitHub-hosted runner no earlier job wrote.
	runnerDir, cacheDir := hookRunnerForJob(t, jobFields("jfrog/curate", "actions/checkout"), map[string]string{
		"pages/a_1.log": setupLine("jfrog", "curate", "v1", testShaSelf) + setupLine("actions", "checkout", "v4", testShaV4),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("jfrog", "curate", testShaSelf) +
			cachedLine("actions", "checkout", testShaV4),
	}, []string{"jfrog/curate/v1", "actions/checkout/v4"})
	decider := &refRecorder{}
	cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(decider).SetFromPre(true)
	cmd.runnerDirFinder = func() (string, error) { return runnerDir, nil }

	_, err := captureReport(t, cmd)

	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{
		"jfrog/curate@v1":     {SHA: testShaSelf, BySHA: true},
		"actions/checkout@v4": {SHA: testShaV4, BySHA: true},
	}, factsOf(decider.refs))
}

func TestCurationActionsCommand_Run_HookModeWithoutARunnerDirFails(t *testing.T) {
	pinRunnerEnv(t, testGithubRepo, "", "")
	_, cacheDir := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}.build(t)
	cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetCallerMode(githubactions.ModeHook, "").SetDecider(&scriptedDecider{})
	_, err := captureReport(t, cmd)
	assert.ErrorContains(t, err, "runner directory")
}

func TestCurationActionsCommand_Run_FromPreWithoutAFinderComparesByContent(t *testing.T) {
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
	t.Setenv(stepSummaryEnvVar, "")
	_, cacheDir := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}.build(t)
	decider := &refRecorder{}
	cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(decider).SetFromPre(true)
	cmd.runnerDirFinder = nil
	_, err := captureReport(t, cmd)
	require.NoError(t, err)
	assert.Equal(t, map[string]refFacts{"actions/checkout@v4": {Reason: githubactions.ReasonNoSHA}}, factsOf(decider.refs))
}
