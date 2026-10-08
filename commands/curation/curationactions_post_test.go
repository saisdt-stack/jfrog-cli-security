package curation

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jfrog/jfrog-cli-security/commands/curation/githubactions"
)

// readSavedState parses a GITHUB_STATE file as the runner does (name=value, or name<<delimiter with
// the value on the lines up to the delimiter) and returns the values by name.
func readSavedState(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	state := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if name, delimiter, ok := strings.Cut(line, "<<"); ok {
			var value []string
			for scanner.Scan() && scanner.Text() != delimiter {
				value = append(value, scanner.Text())
			}
			state[name] = strings.Join(value, "\n")
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		require.True(t, ok, "GITHUB_STATE line %q is neither name=value nor name<<delimiter", line)
		state[name] = value
	}
	require.NoError(t, scanner.Err())
	return state
}

// preJob is a pre's runner: our action first, then checkout, both logged with their SHAs.
func preJob(t *testing.T, worker string) (runnerDir, cacheDir string) {
	t.Helper()
	return hookRunnerForJob(t, jobFields("jfrog/curate", "actions/checkout"), map[string]string{
		"pages/a_1.log":                  setupLine("jfrog", "curate", "v1", testShaSelf) + setupLine("actions", "checkout", "v4", testShaV4),
		"Worker_20261005-050652-utc.log": workerStart + worker,
	}, []string{"jfrog/curate/v1", "actions/checkout/v4"})
}

// manyLoggedRefs is n actions logged by SHA, enough of them that their keys overflow what a step can save.
func manyLoggedRefs(n int) []githubactions.ActionRef {
	var refs []githubactions.ActionRef
	for i := range n {
		refs = append(refs, githubactions.ActionRef{Owner: "owner-with-a-long-name", Repo: fmt.Sprintf("repository-%04d", i),
			RunnerSHA: testShaV4, Verification: githubactions.VerifyLoggedSHA})
	}
	return refs
}

func TestCurationActionsCommand_Run_FromPreSavesWhatItDecided(t *testing.T) {
	tests := []struct {
		name string
		// trustedRunner: the pre finds preJob's hosted runner, whose Worker log names both actions by SHA,
		// checkout in mixed case; false: no runner is visible, so the cache (actions/checkout/v4) is walked.
		trustedRunner bool
		// discovered, when > 0, hands Run that many refs through a fixedDiscoverer instead.
		discovered  int
		wantDecided []string // the keys saved for the post; nil: the decided set is not saved
		wantTrusted string   // the saved JFROG_CURATION_LOGS_TRUSTED
	}{
		{name: "verify when the pre decides then every decided commit is saved for the post", trustedRunner: true,
			wantDecided: []string{"actions/checkout@" + testShaV4, "jfrog/curate@" + testShaSelf}, wantTrusted: "true"},
		{name: "verify when the pre cannot find the runner then it saves each action by ref and that its logs were not trusted",
			wantDecided: []string{"actions/checkout@v4"}, wantTrusted: "false"},
		{name: "verify when the decided set is too large then nothing is saved", discovered: 2000, wantTrusted: "false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
			t.Setenv(githubactions.RunnerEnvironmentEnvVar, "github-hosted")
			t.Setenv(stepSummaryEnvVar, "")
			statePath := filepath.Join(t.TempDir(), "save_state")
			require.NoError(t, os.WriteFile(statePath, []byte("other=kept\n"), 0o644))
			t.Setenv(githubStateEnvVar, statePath)
			var runnerDir, cacheDir string
			if tt.trustedRunner {
				runnerDir, cacheDir = preJob(t, savedLine("jfrog", "curate", testShaSelf)+savedLine("Actions", "Checkout", testShaV4))
			} else {
				_, cacheDir = runnerSpec{cacheDirs: []string{"actions/checkout/v4"}}.build(t)
			}
			cmd := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(&scriptedDecider{}).SetFromPre(true)
			if tt.discovered > 0 {
				cmd.SetActionDiscoverer(fixedDiscoverer{refs: manyLoggedRefs(tt.discovered)})
			}
			cmd.runnerDirFinder = nil
			if tt.trustedRunner {
				cmd.runnerDirFinder = func() (string, error) { return runnerDir, nil }
			}

			report, err := captureReport(t, cmd)

			require.NoError(t, err)
			state := readSavedState(t, statePath)
			assert.Equal(t, "kept", state["other"], "what other steps saved must be kept")
			assert.Equal(t, tt.wantTrusted, state[logsTrustedStateName])
			decided, saved := state[decidedStateName]
			assert.Equal(t, tt.wantDecided != nil, saved, "state: %v", state)
			assert.ElementsMatch(t, tt.wantDecided, strings.Fields(decided))
			assert.Equal(t, tt.wantDecided == nil, strings.Contains(report, "post will decide every"), "report: %s", report)
		})
	}
}

// testShaLate is the commit of an action the runner fetched after the pre step.
const testShaLate = "1a7e1a7e1a7e1a7e1a7e1a7e1a7e1a7e1a7e1a7e"

func TestCurationActionsCommand_Run_FromPostReportsActionsFetchedAfterThePre(t *testing.T) {
	worker := savedLine("jfrog", "curate", testShaSelf) + savedLine("actions", "checkout", testShaV4) +
		savedLine("evil", "late", testShaLate)
	tests := []struct {
		name      string
		saved     *string  // STATE_JFROG_CURATION_DECIDED; nil leaves it unset
		rejected  []string // keys the scriptedDecider rejects
		wantAsked []string
		wantErr   bool
		// wantNotSavedWarning: the post warns that the pre's decisions were not saved.
		wantNotSavedWarning bool
		// wantNote is the note every row the post decided carries: fetchedLateNote when the pre saved SHAs,
		// notBySHANote when it saved none, so whether a commit came late is unknown.
		wantNote string
	}{
		{
			name:      "verify when the Worker log gained a rejected action since the pre then it is reported and fails the post",
			saved:     new("jfrog/curate@" + testShaSelf + "\nactions/checkout@" + testShaV4),
			rejected:  []string{"evil/late@" + testShaLate},
			wantAsked: []string{"evil/late@" + testShaLate},
			wantErr:   true,
			wantNote:  fetchedLateNote,
		},
		{
			name:      "verify when the action fetched since the pre is approved then the post passes",
			saved:     new("jfrog/curate@" + testShaSelf + "\r\nActions/Checkout@" + strings.ToUpper(testShaV4)),
			wantAsked: []string{"evil/late@" + testShaLate},
			wantNote:  fetchedLateNote,
		},
		{
			name:                "verify when the pre saved nothing then every logged commit is decided",
			wantAsked:           []string{"jfrog/curate@" + testShaSelf, "actions/checkout@" + testShaV4, "evil/late@" + testShaLate},
			wantNotSavedWarning: true,
			wantNote:            notBySHANote,
		},
		{
			name:      "verify when the pre saved no SHA then no commit is labelled as fetched late",
			saved:     new("actions/checkout@v4"),
			wantAsked: []string{"jfrog/curate@" + testShaSelf, "actions/checkout@" + testShaV4, "evil/late@" + testShaLate},
			wantNote:  notBySHANote,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			summary := filepath.Join(t.TempDir(), "step_summary.md")
			t.Setenv(stepSummaryEnvVar, summary)
			if tt.saved != nil {
				t.Setenv(postStateEnvVar, *tt.saved)
			} else {
				t.Setenv(postStateEnvVar, "")
				require.NoError(t, os.Unsetenv(postStateEnvVar))
			}
			runnerDir, _ := preJob(t, worker)
			decider := &scriptedDecider{rejected: tt.rejected}
			cmd := NewCurationActionsCommand().SetDecider(decider).SetFromPost(true)
			cmd.runnerDirFinder = func() (string, error) { return runnerDir, nil }

			report, err := captureReport(t, cmd)

			assert.Equal(t, tt.wantErr, err != nil, "Run() error = %v", err)
			assert.ElementsMatch(t, tt.wantAsked, decider.asked)
			assert.Equal(t, len(tt.wantAsked), strings.Count(report, tt.wantNote+" |"), "every post row carries %q:\n%s", tt.wantNote, report)
			otherNote := notBySHANote
			if tt.wantNote == notBySHANote {
				otherNote = fetchedLateNote
			}
			assert.NotContains(t, report, otherNote)
			assert.Equal(t, tt.wantNotSavedWarning, strings.Contains(report, "decided by the pre were not saved"), "report: %s", report)
			content, readErr := os.ReadFile(summary)
			require.NoError(t, readErr)
			assert.Contains(t, string(content), "| evil/late |")
			assert.True(t, folderExists(t, filepath.Join(runnerDir, "_work", "_actions", "actions", "checkout", "v4")),
				"the post cannot undo what ran, so it removes nothing")
		})
	}
}

func TestCurationActionsCommand_Run_FromPostWithAnInvisibleRunnerHasNothingToCheck(t *testing.T) {
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(stepSummaryEnvVar, "")
	decider := &scriptedDecider{}
	cmd := NewCurationActionsCommand().SetDecider(decider).SetFromPost(true)
	cmd.runnerDirFinder = func() (string, error) { return "", githubactions.ErrRunnerNotVisible }

	report, err := captureReport(t, cmd)

	require.NoError(t, err)
	assert.Empty(t, decider.asked)
	assert.Contains(t, report, "[Warn]")
}

func TestCurationActionsCommand_Run_FromPostDecidesOnlyWhatThePreSaved(t *testing.T) {
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(githubactions.ActionRepositoryEnvVar, "jfrog/curate")
	t.Setenv(githubactions.RunnerEnvironmentEnvVar, "github-hosted")
	t.Setenv(stepSummaryEnvVar, "")
	statePath := filepath.Join(t.TempDir(), "save_state")
	t.Setenv(githubStateEnvVar, statePath)
	runnerDir, cacheDir := preJob(t, savedLine("jfrog", "curate", testShaSelf)+savedLine("actions", "checkout", testShaV4))
	pre := NewCurationActionsCommand().SetActionsCacheDir(cacheDir).SetDecider(&scriptedDecider{}).SetFromPre(true)
	pre.runnerDirFinder = func() (string, error) { return runnerDir, nil }
	_, err := captureReport(t, pre)
	require.NoError(t, err)
	worker := filepath.Join(runnerDir, "_diag", "Worker_20261005-050652-utc.log")
	f, err := os.OpenFile(worker, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(savedLine("evil", "late", testShaLate))
	require.NoError(t, errors.Join(err, f.Close()))
	t.Setenv(postStateEnvVar, readSavedState(t, statePath)[decidedStateName])
	decider := &scriptedDecider{}
	post := NewCurationActionsCommand().SetDecider(decider).SetFromPost(true)
	post.runnerDirFinder = func() (string, error) { return runnerDir, nil }

	_, err = captureReport(t, post)

	require.NoError(t, err)
	assert.Equal(t, []string{"evil/late@" + testShaLate}, slices.Clone(decider.asked))
}

func TestCurationActionsCommand_Run_FromPostRowsAreNotLabelledUnpaired(t *testing.T) {
	// The decider labels a commit with no ref and no folder "unpaired logged commit"; a post row is
	// labelled only by why the post decided it, so it must not look unpaired.
	pinRunnerEnv(t, testGithubRepo, "", "")
	t.Setenv(stepSummaryEnvVar, "")
	t.Setenv(postStateEnvVar, "jfrog/curate@"+testShaSelf)
	runnerDir, _ := preJob(t, savedLine("jfrog", "curate", testShaSelf)+savedLine("actions", "checkout", testShaV4))
	decider := &refRecorder{}
	cmd := NewCurationActionsCommand().SetDecider(decider).SetFromPost(true)
	cmd.runnerDirFinder = func() (string, error) { return runnerDir, nil }

	report, err := captureReport(t, cmd)

	require.NoError(t, err)
	require.Len(t, decider.refs, 1)
	got := decider.refs[0]
	assert.False(t, got.Unpaired(), "post ref %+v", got)
	assert.Equal(t, testShaV4, got.RunnerSHA)
	assert.Equal(t, githubactions.VerifyLoggedSHA, got.Verification)
	assert.Contains(t, report, "| actions/checkout | "+testShaV4+" | Approved | fetched after the job started |")
}

func TestCurationActionsCommand_Run_FromPostSaysWhenItsLogsProveNothing(t *testing.T) {
	tests := []struct {
		name string
		// trusted is STATE_JFROG_CURATION_LOGS_TRUSTED; nil leaves it unset.
		trusted     *string
		wantWarning bool
	}{
		{name: "verify when the pre trusted its logs then finding nothing late is not flagged", trusted: new("true")},
		{name: "verify when the pre did not trust its logs then finding nothing late is flagged", trusted: new("false"), wantWarning: true},
		{name: "verify when the pre did not say whether it trusted its logs then finding nothing late is flagged", wantWarning: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinRunnerEnv(t, testGithubRepo, "", "")
			t.Setenv(stepSummaryEnvVar, "")
			t.Setenv(postStateEnvVar, "jfrog/curate@"+testShaSelf+"\nactions/checkout@"+testShaV4)
			t.Setenv(postTrustEnvVar, "")
			if tt.trusted != nil {
				t.Setenv(postTrustEnvVar, *tt.trusted)
			} else {
				require.NoError(t, os.Unsetenv(postTrustEnvVar))
			}
			runnerDir, _ := preJob(t, savedLine("jfrog", "curate", testShaSelf)+savedLine("actions", "checkout", testShaV4))
			cmd := NewCurationActionsCommand().SetDecider(&scriptedDecider{}).SetFromPost(true)
			cmd.runnerDirFinder = func() (string, error) { return runnerDir, nil }

			report, err := captureReport(t, cmd)

			require.NoError(t, err)
			assert.Contains(t, report, "fetched no action after the job started")
			assert.Equal(t, tt.wantWarning, strings.Contains(report, "is not proof"), "report: %s", report)
		})
	}
}
