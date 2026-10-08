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
	tests := []struct {
		name      string
		cacheDirs []string // owner/repo/ref entries written with their .completed watermark
		bareDirs  []string // owner/repo/ref entries written without one, which discovery cannot account for
		want      map[string]refFacts
		wantErr   string // substring of Discover's error; "" when it succeeds
	}{
		{
			name:      "verify when the cache holds actions then each is returned once",
			cacheDirs: []string{"actions/checkout/v4", "actions/setup-go/v5"},
			want:      map[string]refFacts{"actions/checkout@v4": {}, "actions/setup-go@v5": {}},
		},
		{
			name:    "verify when the cache is empty then it reports the cache as unreadable",
			wantErr: "cannot read the GitHub Actions cache",
		},
		{
			name:     "verify when an entry cannot be accounted for then it fails",
			bareDirs: []string{"actions/checkout/v4"},
			wantErr:  "cannot account for every entry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, cacheDir := runnerSpec{cacheDirs: tt.cacheDirs}.build(t)
			for _, dir := range tt.bareDirs {
				require.NoError(t, os.MkdirAll(filepath.Join(cacheDir, filepath.FromSlash(dir)), 0o755))
			}
			refs, err := cacheWalkDiscoverer{actionsCacheDir: cacheDir}.Discover()
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, refs, len(tt.want), "Discover() refs")
			assert.Equal(t, tt.want, factsOf(refs))
		})
	}
}

func TestCurationActionsCommand_Run_DiscoveryFailure(t *testing.T) {
	tests := []struct {
		name       string
		discoverer actionDiscoverer         // nil: Run picks the discoverer for mode
		mode       githubactions.CallerMode // the zero value runs as a step
		wantErr    string                   // substring of Run's error
	}{
		{
			name:       "verify when the discoverer fails then Run returns its error and decides nothing",
			discoverer: fixedDiscoverer{err: errors.New("discovery failed")},
			wantErr:    "discovery failed",
		},
		{
			name:    "verify when run as the hook without a runner directory then it fails before deciding",
			mode:    githubactions.ModeHook,
			wantErr: "runner directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			_, cacheDir := runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}.build(t)
			decider := &scriptedDecider{}
			cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(decider).SetCallerMode(tt.mode, "")
			if tt.discoverer != nil {
				cmd.SetActionDiscoverer(tt.discoverer)
			}
			_, err := captureReport(t, cmd)
			assert.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, decider.asked, "no action may be decided when discovery failed")
		})
	}
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

// withDiag returns a copy of diag with name set to content.
func withDiag(diag map[string]string, name, content string) map[string]string {
	out := maps.Clone(diag)
	out[name] = content
	return out
}

func TestRunnerLogDiscoverer(t *testing.T) {
	// codeloadSave is a Save line followed by the request line a real Worker log carries after it.
	codeloadSave := func(repo, sha string) string {
		return savedLine("actions", repo, sha) +
			"Request URL: https://codeload.github.com/actions/" + repo + "/tar.gz/" + sha + " X-GitHub-Request-Id: A\n"
	}
	checkoutAndNode := jobFields("actions/checkout", "actions/setup-node")
	checkoutTwiceAndNode := workerStart + savedLine("actions", "checkout", testShaV4) + savedLine("actions", "checkout", testShaV3) +
		savedLine("actions", "setup-node", testShaNode)
	attackerFirstDiag := map[string]string{
		"pages/a_1.log": setupLine("attacker", "first", "v1", testShaEvil) + setupLine("jfrog", "curate", "v1", testShaSelf),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("attacker", "first", testShaEvil) +
			savedLine("jfrog", "curate", testShaSelf),
	}
	checkoutTwice := workerStart + savedLine("actions", "checkout", testShaV4) + savedLine("actions", "checkout", testShaV3)

	tests := []struct {
		name      string
		job       string            // extra job-message fields (see jobFields); "" leaves the timeline and steps unknown
		diag      map[string]string // path under <runner>/_diag -> content; nil leaves no _diag folder
		cacheDirs []string          // owner/repo/ref entries in _actions
		sameAge   []string          // cacheDirs whose watermarks get one time, so age cannot pair them
		mode      githubactions.CallerMode
		self      string // the running action's owner/repo in githubactions.ModePre
		want      map[string]refFacts
		wantInLog map[string]int // substring -> exact number of times the log holds it
	}{
		// The fast path, and what keeps it off.
		{
			name: "verify when the setup lines cover every fetched action of a trusted job then each is decided by its logged SHA without walking the cache",
			job:  jobFields("actions/checkout"),
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "checkout", "v3", testShaV3),
				"Worker_20261005-050652-utc.log": checkoutTwice,
			},
			mode: githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4": {SHA: testShaV4, BySHA: true},
				"actions/checkout@v3": {SHA: testShaV3, BySHA: true},
			},
		},
		{
			name:      "verify when the caller mode is unset then the logs never decide by SHA",
			job:       checkoutAndNode,
			diag:      trustedHookDiag,
			cacheDirs: trustedHookCacheDirs,
			want: map[string]refFacts{
				"actions/checkout@v4":   {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
				"actions/setup-node@v4": {SHA: testShaNode, Reason: githubactions.ReasonLogUntrusted},
			},
		},
		{
			name: "verify when the setup lines cover only some fetched actions then the cache walk recovers the rest",
			diag: map[string]string{
				"blocks/b_1.log":                 setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": checkoutTwice,
			},
			cacheDirs: []string{"actions/checkout/v4", "actions/checkout/v3"},
			want: map[string]refFacts{
				"actions/checkout@v4": {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
				"actions/checkout@v3": {SHA: testShaV3, Reason: githubactions.ReasonLogUntrusted},
			},
		},
		{
			name:      "verify when the setup buffer is gone then a ref paired by elimination is decided by its logged SHA and only the ambiguous pair is compared",
			job:       checkoutAndNode,
			diag:      map[string]string{"Worker_20261005-050652-utc.log": checkoutTwiceAndNode},
			cacheDirs: []string{"actions/checkout/v4", "actions/checkout/v3", "actions/setup-node/v4"},
			sameAge:   []string{"actions/checkout/v4", "actions/checkout/v3"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
				"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
				"actions/setup-node@v4":         {SHA: testShaNode, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
		// Provenance rules: the whole job is compared by content.
		{
			name:      "verify when an earlier job planted a newer Worker log then every action is compared by content",
			job:       checkoutAndNode,
			diag:      withDiag(trustedHookDiag, plantedWorkerLog, "[x INFO Worker] continued\n"),
			cacheDirs: trustedHookCacheDirs,
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":   {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
				"actions/setup-node@v4": {SHA: testShaNode, Reason: githubactions.ReasonLogUntrusted},
			},
		},
		{
			name: "verify when an earlier job planted a Worker log that sorts last then the runner's SHA still decides",
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": workerStart + codeloadSave("checkout", testShaV4),
				"Worker_29991231-235959-utc.log": workerStart + codeloadSave("checkout", testShaV3),
			},
			cacheDirs: []string{"actions/checkout/v4"},
			want:      map[string]refFacts{"actions/checkout@v4": {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted}},
		},
		{
			name: "verify when the hook cannot read the job message then every action is compared by content",
			job:  `, "timeline": {"id": "a"}, "steps": 5`,
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4),
			},
			cacheDirs: []string{"actions/checkout/v4"},
			mode:      githubactions.ModeHook,
			want:      map[string]refFacts{"actions/checkout@v4": {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted}},
			wantInLog: map[string]int{"cannot be trusted": 1, "the job message could not be read": 1},
		},
		{
			name:      "verify when another action runs before this pre then the job is compared by content",
			job:       jobFields("attacker/first", "jfrog/curate"),
			diag:      attackerFirstDiag,
			cacheDirs: []string{"attacker/first/v1", "jfrog/curate/v1"},
			mode:      githubactions.ModePre,
			self:      "jfrog/curate",
			want: map[string]refFacts{
				"attacker/first@v1": {SHA: testShaEvil, Reason: githubactions.ReasonLogUntrusted},
				"jfrog/curate@v1":   {SHA: testShaSelf, Reason: githubactions.ReasonLogUntrusted},
			},
		},
		{
			name:      "verify when this pre is the first step then the logged SHAs decide",
			job:       jobFields("jfrog/curate", "attacker/first"),
			diag:      attackerFirstDiag,
			cacheDirs: []string{"attacker/first/v1", "jfrog/curate/v1"},
			mode:      githubactions.ModePre,
			self:      "jfrog/curate",
			want: map[string]refFacts{
				"attacker/first@v1": {SHA: testShaEvil, BySHA: true},
				"jfrog/curate@v1":   {SHA: testShaSelf, BySHA: true},
			},
		},
		{
			name:      "verify when the diag folder is unreadable then it falls back to the cache walk",
			cacheDirs: []string{"actions/checkout/v4"},
			mode:      githubactions.ModeHook,
			want:      map[string]refFacts{"actions/checkout@v4": {Reason: githubactions.ReasonLogUntrusted}},
			wantInLog: map[string]int{"[Warn]": 1},
		},
		{
			name:      "verify when the logs name nothing then it falls back to the cache walk",
			diag:      map[string]string{"Worker_20261005-050652-utc.log": workerStart},
			cacheDirs: []string{"actions/checkout/v4"},
			want:      map[string]refFacts{"actions/checkout@v4": {Reason: githubactions.ReasonNoSHA}},
		},
		// Evidence rules: only the action concerned is compared by content.
		{
			name: "verify when a setup line of the job names a SHA the Worker never fetched then only that action is compared",
			job:  checkoutAndNode,
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "setup-node", "v4", testShaEvil),
				"Worker_20261005-050652-utc.log": trustedHookDiag["Worker_20261005-050652-utc.log"],
			},
			cacheDirs: trustedHookCacheDirs,
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":   {SHA: testShaV4, BySHA: true},
				"actions/setup-node@v4": {SHA: testShaNode, Reason: githubactions.ReasonStaleLine},
			},
		},
		{
			name: "verify when a stale setup line is for a repository outside the job then nothing changes",
			job:  checkoutAndNode,
			diag: withDiag(trustedHookDiag, "pages/a_1.log", trustedHookDiag["pages/a_1.log"]+
				setupLine("attackerpre", "evil", "v9", testShaEvil)),
			cacheDirs: trustedHookCacheDirs,
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":   {SHA: testShaV4, BySHA: true},
				"actions/setup-node@v4": {SHA: testShaNode, BySHA: true},
			},
		},
		{
			name: "verify when an action came from a self-hosted cache whose writability is unknown then only that action is compared",
			job:  checkoutAndNode,
			diag: withDiag(trustedHookDiag, "Worker_20261005-050652-utc.log", workerStart+cachedLine("actions", "checkout", testShaV4)+
				savedLine("actions", "setup-node", testShaNode)),
			cacheDirs: trustedHookCacheDirs,
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":   {SHA: testShaV4, Reason: githubactions.ReasonCacheSource},
				"actions/setup-node@v4": {SHA: testShaNode, BySHA: true},
			},
			wantInLog: map[string]int{
				"cannot be trusted": 0, // an evidence rule downgrades the action, not the job
				"[Warn]":            1, // one warning names every downgraded action
				`"actions/checkout@v4": ` + githubactions.ReasonCacheSource: 1,
				"setup-node": 0,
			},
		},
		// Pairing refs with logged commits: every logged commit is decided under its own name.
		{
			name:      "verify when a forged setup line hands one action another's SHA then that copy is compared and its own commit is decided by SHA",
			job:       jobFields("aa/aa", "actions/checkout"),
			diag:      swappedSHADiag,
			cacheDirs: swappedSHACacheDirs,
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"aa/aa@v1":             {SHA: testShaV4, Reason: githubactions.ReasonRenamed},
				"actions/checkout@v4":  {SHA: testShaV4, BySHA: true},
				"aa/aa#" + testShaEvil: {SHA: testShaEvil, BySHA: true},
			},
		},
		{
			name: "verify when the cache is walked then a stolen commit is decided under its own name",
			job:  jobFields("aa/aa", "actions/checkout"),
			diag: map[string]string{
				"pages/a_1.log": setupLine("aa", "aa", "v1", testShaV4) + setupLine("actions", "checkout", "v4", testShaV4) +
					setupLine("actions", "checkout", "v3", testShaV3),
				"Worker_20261005-050652-utc.log": checkoutTwice + savedLine("aa", "aa", testShaEvil),
			},
			cacheDirs: []string{"aa/aa/v1", "actions/checkout/v4", "actions/checkout/v3"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"aa/aa@v1":                      {SHA: testShaV4, Reason: githubactions.ReasonRenamed},
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4}},
				"actions/checkout@v3":           {SHA: testShaV3, BySHA: true},
				"aa/aa#" + testShaEvil:          {SHA: testShaEvil, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
			},
		},
		{
			name: "verify when the setup lines account for the job then a stolen commit is decided under its own name",
			job:  jobFields("aa/aa", "actions/checkout"),
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("aa", "aa", "v1", testShaV4) + setupLine("actions", "checkout", "v3", testShaV3),
				"Worker_20261005-050652-utc.log": checkoutTwice,
			},
			cacheDirs: []string{"aa/aa/v1", "actions/checkout/v3"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"aa/aa@v1":                      {SHA: testShaV4, Reason: githubactions.ReasonRenamed},
				"actions/checkout@v3":           {SHA: testShaV3, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
			},
		},
		{
			name: "verify when the job is untrusted then a logged commit no folder holds is still decided by SHA",
			job:  jobFields("actions/checkout"),
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": checkoutTwice,
				plantedWorkerLog:                 "[x INFO Worker] continued\n",
			},
			cacheDirs: []string{"actions/checkout/v4"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":           {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
		{
			name: "verify when a repository was renamed then its folder is compared and the commit is decided under the new name",
			diag: map[string]string{
				"pages/a_1.log": setupLine("oldowner", "tool", "v1", testShaV4),
				"Worker_20261005-050652-utc.log": workerStart +
					workerLine("Save archive 'https://api.github.com/repos/newowner/tool/tarball/"+testShaV4+"' into x"),
			},
			cacheDirs: []string{"oldowner/tool/v1"},
			want: map[string]refFacts{
				"oldowner/tool@v1":           {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
				"newowner/tool#" + testShaV4: {SHA: testShaV4, BySHA: true},
			},
		},
		{
			name: "verify when the runner fetched an action that is not in the cache then its commit is decided by SHA",
			// A renamed repository with no setup line, or a Worker line a remote composite's docker://
			// image forged: either way the commit is decided by its SHA and every folder by its own.
			diag: map[string]string{
				"Worker_20261005-050652-utc.log": workerStart + codeloadSave("checkout", testShaV4) + codeloadSave("setup-node", testShaV3),
			},
			cacheDirs: []string{"actions/checkout/v4"},
			want: map[string]refFacts{
				"actions/checkout@v4":             {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted},
				"actions/setup-node#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
			wantInLog: map[string]int{`"actions/setup-node@` + testShaV3 + `"`: 1},
		},
		// Logged commits handed to the decider for a ref the logs could not pair.
		{
			name: "verify when the job is trusted then only the unpaired refs carry their repository's logged commits",
			job:  checkoutAndNode,
			diag: map[string]string{
				"pages/a_1.log": setupLine("actions", "setup-node", "v4", testShaNode),
				"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", strings.ToUpper(testShaV4)) +
					savedLine("actions", "checkout", testShaV3) + savedLine("actions", "setup-node", testShaNode),
			},
			cacheDirs: []string{"actions/checkout/v4", "actions/checkout/v3", "actions/setup-node/v4"},
			sameAge:   []string{"actions/checkout/v4", "actions/checkout/v3"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
				"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV4, testShaV3}},
				"actions/setup-node@v4":         {SHA: testShaNode, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
		{
			name: "verify when a logged commit came from a writable cache on a self-hosted runner then no ref is paired with it through the refs list",
			job:  checkoutAndNode,
			diag: map[string]string{
				"pages/a_1.log": setupLine("actions", "setup-node", "v4", testShaNode),
				"Worker_20261005-050652-utc.log": workerStart + cachedLine("actions", "checkout", testShaV4) +
					savedLine("actions", "checkout", testShaV3) + savedLine("actions", "setup-node", testShaNode),
			},
			cacheDirs: []string{"actions/checkout/v4", "actions/checkout/v3", "actions/setup-node/v4"},
			sameAge:   []string{"actions/checkout/v4", "actions/checkout/v3"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV3}},
				"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV3}},
				"actions/setup-node@v4":         {SHA: testShaNode, BySHA: true},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
		{
			name: "verify when the job is untrusted then no ref carries logged commits",
			job:  checkoutAndNode,
			diag: map[string]string{
				"pages/a_1.log": setupLine("actions", "setup-node", "v4", testShaNode),
				"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", strings.ToUpper(testShaV4)) +
					savedLine("actions", "checkout", testShaV3) + savedLine("actions", "setup-node", testShaNode),
				plantedWorkerLog: "[x INFO Worker] continued\n",
			},
			cacheDirs: []string{"actions/checkout/v4", "actions/checkout/v3", "actions/setup-node/v4"},
			sameAge:   []string{"actions/checkout/v4", "actions/checkout/v3"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":           {Reason: githubactions.ReasonNoSHA},
				"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA},
				"actions/setup-node@v4":         {SHA: testShaNode, Reason: githubactions.ReasonLogUntrusted},
				"actions/checkout#" + testShaV4: {SHA: testShaV4, BySHA: true},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
		{
			name: "verify when another folder already holds a logged commit then the unpaired refs are offered only the rest",
			job:  jobFields("actions/checkout"),
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": checkoutTwice,
			},
			cacheDirs: []string{"actions/checkout/v4", "actions/checkout/v3", "actions/checkout/main"},
			sameAge:   []string{"actions/checkout/v3", "actions/checkout/main"},
			mode:      githubactions.ModeHook,
			want: map[string]refFacts{
				"actions/checkout@v4":           {SHA: testShaV4, BySHA: true},
				"actions/checkout@v3":           {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV3}},
				"actions/checkout@main":         {Reason: githubactions.ReasonNoSHA, Logged: []string{testShaV3}},
				"actions/checkout#" + testShaV3: {SHA: testShaV3, BySHA: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerDir, cacheDir := hookRunnerForJob(t, tt.job, tt.diag, tt.cacheDirs)
			same := time.Now()
			for _, dir := range tt.sameAge {
				require.NoError(t, os.Chtimes(filepath.Join(cacheDir, filepath.FromSlash(dir)+".completed"), same, same))
			}
			var refs []githubactions.ActionRef
			var err error
			out := captureLog(t, log.INFO, func() {
				refs, err = runnerLogDiscoverer{runnerDir: runnerDir, actionsCacheDir: cacheDir, mode: tt.mode, self: tt.self}.Discover()
			})
			require.NoError(t, err)
			assert.Len(t, refs, len(tt.want), "Discover() refs")
			assert.Equal(t, tt.want, factsOf(refs))
			for s, n := range tt.wantInLog {
				assert.Equal(t, n, strings.Count(out, s), "times the log holds %q:\n%s", s, out)
			}
		})
	}
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

// trustedHookDiag and trustedHookCacheDirs are a job a hook can trust: checkout and setup-node, each
// logged by the Worker and named by a setup line.
var (
	trustedHookDiag = map[string]string{
		"pages/a_1.log": setupLine("actions", "checkout", "v4", testShaV4) + setupLine("actions", "setup-node", "v4", testShaNode),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) +
			savedLine("actions", "setup-node", testShaNode),
	}
	trustedHookCacheDirs = []string{"actions/checkout/v4", "actions/setup-node/v4"}
)

// trustedHookJob lays out trustedHookDiag for the job of checkout and setup-node.
func trustedHookJob(t *testing.T) (runnerDir, cacheDir string) {
	t.Helper()
	return hookRunnerForJob(t, jobFields("actions/checkout", "actions/setup-node"), trustedHookDiag, trustedHookCacheDirs)
}

// swappedSHADiag and swappedSHACacheDirs are a job whose aa/aa folder holds the commit the Worker
// logged as testShaEvil, while a forged setup line hands it testShaV4, which the Worker logged for
// actions/checkout. aa/aa sorts first, so the forged line is read before checkout's own.
var (
	swappedSHADiag = map[string]string{
		"pages/a_1.log": setupLine("aa", "aa", "v1", testShaV4) + setupLine("actions", "checkout", "v4", testShaV4),
		"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4) +
			savedLine("aa", "aa", testShaEvil),
	}
	swappedSHACacheDirs = []string{"aa/aa/v1", "actions/checkout/v4"}
)

// plantedWorkerLog is a Worker log newer than the job's own and without a start line, which a job
// could have written.
const plantedWorkerLog = "Worker_20261005-060000-utc.log"

// checkoutArchiveCache is an archive cache holding actions/checkout at testShaV4, made read-only by
// its owner, the user running the test, when readOnly is set. Write permission is restored on cleanup
// so the temp dir can be removed.
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
			assert.Len(t, refs, 2, "Discover() refs")
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

func TestCurationActionsCommand_Run_HookModeForgedSetupLineCannotApproveAnEvilCopy(t *testing.T) {
	// The real decider: the forged line hands aa/aa's evil copy checkout's approved SHA, so the copy
	// is compared at that SHA and fails, and the commit the Worker really fetched for aa/aa is decided too.
	const vcsRepo = "github-vcs"
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(stepSummaryEnvVar, "")
	runnerDir, cacheDir := hookRunnerForJob(t, jobFields("aa/aa", "actions/checkout"), swappedSHADiag, swappedSHACacheDirs)
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

func TestCurationActionsCommand_Run_CallerMode(t *testing.T) {
	// The workspace holds a previous run's checkout at job start, so its workflow file may not be this
	// job's: only a step attributes from it.
	const previousRunWorkflow = "jobs:\n  build:\n    steps:\n      - uses: jfrog/curate@v1\n      - uses: actions/checkout@v4\n"
	tests := []struct {
		name                string
		mode                githubactions.CallerMode // the zero value runs as a step; ModePre runs with --from-pre
		runnerVisible       bool                     // ModePre only: the finder returns the runner rather than ErrRunnerNotVisible
		job                 string                   // extra job-message fields (see jobFields)
		diag                map[string]string        // path under <runner>/_diag -> content
		cacheDirs           []string                 // owner/repo/ref entries in _actions
		wantRefs            map[string]refFacts      // what the decider was asked, and how each was to be verified
		wantParent          bool                     // the report attributes each action to the workflow step that uses it
		wantInReport        []string
		wantSummaryAppended bool // the report is appended to GITHUB_STEP_SUMMARY; otherwise the file is left as it was
	}{
		{
			name:       "verify when run as a step then it attributes from the workflow file and leaves the step summary to setup-jfrog-cli",
			cacheDirs:  []string{"jfrog/curate/v1", "actions/checkout/v4"},
			wantRefs:   map[string]refFacts{"jfrog/curate@v1": {}, "actions/checkout@v4": {}},
			wantParent: true,
		},
		{
			name: "verify when run as the hook then it does not attribute from the workspace and appends the report to the step summary",
			mode: githubactions.ModeHook,
			diag: map[string]string{
				"pages/a_1.log":                  setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": workerStart + savedLine("actions", "checkout", testShaV4),
			},
			cacheDirs:           []string{"actions/checkout/v4"},
			wantRefs:            map[string]refFacts{"actions/checkout@v4": {SHA: testShaV4, Reason: githubactions.ReasonLogUntrusted}},
			wantInReport:        []string{"Local composite actions (uses: ./...) are not curated"},
			wantSummaryAppended: true,
		},
		{
			name:                "verify when a pre cannot see the runner then every action is compared by content",
			mode:                githubactions.ModePre,
			cacheDirs:           []string{"jfrog/curate/v1", "actions/checkout/v4"},
			wantRefs:            map[string]refFacts{"jfrog/curate@v1": {Reason: githubactions.ReasonNoSHA}, "actions/checkout@v4": {Reason: githubactions.ReasonNoSHA}},
			wantInReport:        []string{"not visible"},
			wantSummaryAppended: true,
		},
		{
			// checkout came from the runner's action cache, which on a GitHub-hosted runner no earlier job wrote.
			name:          "verify when a pre is the job's first step on a hosted runner then every action is decided by its logged SHA",
			mode:          githubactions.ModePre,
			runnerVisible: true,
			job:           jobFields("jfrog/curate", "actions/checkout"),
			diag: map[string]string{
				"pages/a_1.log": setupLine("jfrog", "curate", "v1", testShaSelf) + setupLine("actions", "checkout", "v4", testShaV4),
				"Worker_20261005-050652-utc.log": workerStart + savedLine("jfrog", "curate", testShaSelf) +
					cachedLine("actions", "checkout", testShaV4),
			},
			cacheDirs:           []string{"jfrog/curate/v1", "actions/checkout/v4"},
			wantRefs:            map[string]refFacts{"jfrog/curate@v1": {SHA: testShaSelf, BySHA: true}, "actions/checkout@v4": {SHA: testShaV4, BySHA: true}},
			wantSummaryAppended: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
			t.Setenv(githubactions.RunnerEnvironmentEnvVar, "github-hosted")
			summary := filepath.Join(t.TempDir(), "step_summary.md")
			require.NoError(t, os.WriteFile(summary, []byte("earlier hook\n"), 0o644))
			t.Setenv(stepSummaryEnvVar, summary)
			workingDir, _ := runnerSpec{workflowYAML: previousRunWorkflow}.build(t)
			runnerDir, cacheDir := hookRunnerForJob(t, tt.job, tt.diag, tt.cacheDirs)
			decider := &refRecorder{}
			cmd := NewCurationActionsCommand().SetWorkingDir(workingDir).SetActionsCacheDir(cacheDir).SetDecider(decider).
				SetWorkflowFile(filepath.Join(workingDir, ".github", "workflows", "ci.yml")).SetJobID("build")
			switch tt.mode {
			case githubactions.ModeHook:
				cmd.SetRunnerDir(runnerDir).SetCallerMode(githubactions.ModeHook, "")
			case githubactions.ModePre:
				cmd.SetFromPre(true)
				cmd.runnerDirFinder = func() (string, error) {
					if !tt.runnerVisible {
						return "", githubactions.ErrRunnerNotVisible
					}
					return runnerDir, nil
				}
			}

			report, err := captureReport(t, cmd)

			require.NoError(t, err)
			assert.Len(t, decider.refs, len(tt.wantRefs), "refs decided")
			assert.Equal(t, tt.wantRefs, factsOf(decider.refs))
			assert.Equal(t, tt.wantParent, strings.Contains(report, "| Parent |"), "Run() report:\n%s", report)
			for _, s := range tt.wantInReport {
				assert.Contains(t, report, s)
			}
			content, err := os.ReadFile(summary)
			require.NoError(t, err)
			if tt.wantSummaryAppended {
				assert.True(t, strings.HasPrefix(string(content), "earlier hook\n"), "the summary must be appended, not replace what is there")
				assert.Contains(t, string(content), "| actions/checkout | v4 |")
			} else {
				assert.Equal(t, "earlier hook\n", string(content))
			}
		})
	}
}
