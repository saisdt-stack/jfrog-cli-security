package curation

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
func hookRunner(t *testing.T, diag map[string]string, cacheDirs []string) (runnerDir, cacheDir string) {
	t.Helper()
	runnerDir = t.TempDir()
	for name, content := range diag {
		path := filepath.Join(runnerDir, "_diag", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
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
		return "Save archive 'https://codeload.github.com/actions/" + repo + "/tar.gz/" + sha + "' into x\n" +
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
		assert.Equal(t, githubactions.SourceDownloaded, refs[0].Source)
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
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
		require.NoError(t, err)
		assert.Len(t, refs, 1)
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
		require.Len(t, refs, 2)
		for _, r := range refs {
			assert.Empty(t, r.RunnerSHA)
		}
	})
}

func TestCurationActionsCommand_Run_HookModeReportsTheRunnerSHA(t *testing.T) {
	pinRunnerEnv(t, "octo/repo", "", "")
	runnerDir, cacheDir := hookRunner(t, map[string]string{
		"pages/a_1.log":                  "Download action repository 'actions/checkout@v4' (SHA:" + testShaV4 + ")\n",
		"Worker_20261005-050652-utc.log": "[x INFO Worker] Version: 2.337.0\nSave archive 'https://codeload.github.com/actions/checkout/tar.gz/" + testShaV4 + "' into x\n",
	}, []string{"actions/checkout/v4"})
	cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetRunnerDir(runnerDir).SetDecider(&scriptedDecider{})
	report, err := captureReport(t, cmd)
	require.NoError(t, err)
	assert.Contains(t, report, "| Runner SHA |")
	assert.Contains(t, report, testShaV4)
}

func TestRunnerLogDiscovererAcceptsARenamedRepository(t *testing.T) {
	runnerDir, cacheDir := hookRunner(t, map[string]string{
		"pages/a_1.log":                  "Download action repository 'oldowner/tool@v1' (SHA:" + testShaV4 + ")\n",
		"Worker_20261005-050652-utc.log": "[x INFO Worker] Version: 2.337.0\nSave archive 'https://api.github.com/repos/newowner/tool/tarball/" + testShaV4 + "' into x\n",
	}, []string{"oldowner/tool/v1"})
	refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir}.Discover()
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, testShaV4, refs[0].RunnerSHA)
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
		"Worker_20261005-050652-utc.log": "[x INFO Worker] Version: 2.337.0\nSave archive 'https://codeload.github.com/actions/checkout/tar.gz/" + testShaV4 + "' into x\n",
	}, nil)
	cmd := spec.newCommand(t, writtenWorkflowFile, "build", &scriptedDecider{}).SetRunnerDir(runnerDir)
	report, err := captureReport(t, cmd)
	require.NoError(t, err)
	assert.NotContains(t, report, "| Parent |")
	assert.Contains(t, report, "Not covered: no workflow file was available")
}

func TestRunnerLogDiscovererSetupLineCoverage(t *testing.T) {
	start := "[x INFO Worker] Version: 2.337.0\n"
	save := func(sha string) string {
		return "Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + sha + "' into x\n"
	}
	setup := func(ref, sha string) string {
		return "Download action repository 'actions/checkout@" + ref + "' (SHA:" + sha + ")\n"
	}
	t.Run("verify when the setup lines cover every fetched action then the cache is not walked", func(t *testing.T) {
		runnerDir, _ := hookRunner(t, map[string]string{
			"pages/a_1.log":                  setup("v4", testShaV4) + setup("v3", testShaV3),
			"Worker_20261005-050652-utc.log": start + save(testShaV4) + save(testShaV3),
		}, nil)
		refs, err := runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: filepath.Join(runnerDir, "no-such-cache")}.Discover()
		require.NoError(t, err)
		got := map[string]string{}
		for _, r := range refs {
			got[r.Owner+"/"+r.Repo+"@"+r.Ref] = r.RunnerSHA + " " + string(r.Source)
		}
		assert.Equal(t, map[string]string{
			"actions/checkout@v4": testShaV4 + " Downloaded",
			"actions/checkout@v3": testShaV3 + " Downloaded",
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
