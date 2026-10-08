package githubactions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testRun = RunIdentity{RunID: "37295940958", RunAttempt: "1", Repository: "octo/repo"}
	// testNow is after every Worker log the tests name, except a planted future-dated one.
	testNow     = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	workerStart = workerStartFor(testRun)
)

// workerStartFor is the top of a Worker log for run, laid out as the runner writes it: the start
// line, then the job message whose github context names the run.
func workerStartFor(run RunIdentity) string {
	return "[2026-10-05 05:06:52Z INFO Worker] Version: 2.337.0\n" +
		"[2026-10-05 05:06:52Z INFO Worker] Job message:\n {\n  \"contextData\": {\n    \"github\": {\n      \"t\": 2,\n      \"d\": [\n" +
		"        {\"k\": \"repository\", \"v\": \"" + run.Repository + "\"},\n" +
		"        {\"k\": \"run_id\", \"v\": \"" + run.RunID + "\"},\n" +
		"        {\"k\": \"run_attempt\", \"v\": \"" + run.RunAttempt + "\"}\n" +
		"      ]\n    }\n  }\n}\n"
}

func writeDiag(t *testing.T, files map[string]string) string {
	t.Helper()
	runnerDir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(runnerDir, "_diag", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return runnerDir
}

func TestReadRunnerDiag(t *testing.T) {
	download := func(sha string) string {
		return workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + sha + "' into x")
	}
	t.Run("verify when the setup buffer and one Worker log exist then both are read", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"pages/a_1.log":                  "Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n",
			"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.Equal(t, []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}}, snap.SetupJob)
		assert.Equal(t, []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceSave}}, snap.Worker)
	})
	t.Run("verify when the setup buffer is already gone then the Worker log is still read", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{"Worker_20261005-050652-utc.log": workerStart + download(shaV4)})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.Empty(t, snap.SetupJob)
		assert.Len(t, snap.Worker, 1)
	})
	t.Run("verify when part of the setup buffer cannot be read then the rest and the Worker log are still read", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"pages":                          "a file where the runner keeps a folder, so it cannot be listed",
			"blocks/b_1.log":                 "Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n",
			"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err, "the Worker log does not depend on the setup buffer")
		assert.Equal(t, []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}}, snap.SetupJob)
		assert.Equal(t, []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceSave}}, snap.Worker)
	})
	t.Run("verify when the Worker log rolled over then it reads across a rollover", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"Worker_20261005-044838-utc.log": workerStart + download(shaV3), // an earlier job: must not be read
			"Worker_20261005-050652-utc.log": workerStart + "padding\n",     // this job's first file
			"Worker_20261005-051117-utc.log": download(shaV4),               // rolled file, no start marker
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.Equal(t, []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceSave}}, snap.Worker)
		assert.Equal(t, []string{"Worker_20261005-050652-utc.log", "Worker_20261005-051117-utc.log"}, snap.WorkerFiles)
	})
	t.Run("verify when no Worker log starts a process then it fails", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{"Worker_20261005-051117-utc.log": download(shaV4)})
		_, err := ReadRunnerDiag(dir, testRun, testNow)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Worker] Version:")
	})
	t.Run("verify when a Worker log is dated after now then it is not read", func(t *testing.T) {
		// The runner names a log when its Worker starts, so a later name is one a job planted to sort last.
		dir := writeDiag(t, map[string]string{
			"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
			"Worker_29991231-235959-utc.log": workerStart + download(shaV3),
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.Equal(t, []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceSave}}, snap.Worker)
		assert.Equal(t, []string{"Worker_20261005-050652-utc.log"}, snap.WorkerFiles)
	})
	t.Run("verify when the newest Worker log names another run then it fails", func(t *testing.T) {
		other := RunIdentity{RunID: "1", RunAttempt: "1", Repository: testRun.Repository}
		dir := writeDiag(t, map[string]string{"Worker_20261005-050652-utc.log": workerStartFor(other) + download(shaV4)})
		_, err := ReadRunnerDiag(dir, testRun, testNow)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Worker_20261005-050652-utc.log")
	})
	t.Run("verify when this job's run is unknown then it fails", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{"Worker_20261005-050652-utc.log": workerStart + download(shaV4)})
		_, err := ReadRunnerDiag(dir, RunIdentity{}, testNow)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GITHUB_RUN_ID")
	})
	t.Run("verify when there is no _diag folder then it fails", func(t *testing.T) {
		_, err := ReadRunnerDiag(t.TempDir(), testRun, testNow)
		require.Error(t, err)
	})
}

const jobMessageLine = "[2026-10-05 05:06:52Z INFO Worker] Job message:\n "

func TestParseJobMessage(t *testing.T) {
	t.Run("verify when the job has four kinds of step then they are read in order", func(t *testing.T) {
		facts, err := parseJobMessage(jobMessageLine + `{
  "timeline": {"id": "32ef2141-1ecb-4383-a4cf-d6d9e08a9de6", "changeId": 0, "location": null},
  "steps": [
    {"type": "action", "reference": {"type": "repository", "name": "dattathallam/curate-pre-probe", "ref": "v2", "repositoryType": "GitHub"}, "name": "__curate"},
    {"type": "action", "reference": {"type": "repository", "name": "actions/cache", "ref": "v4", "path": "restore", "repositoryType": "GitHub"}},
    {"type": "action", "reference": {"type": "containerRegistry", "image": "alpine:3.20"}, "name": "__alpine_3_20"},
    {"type": "action", "reference": {"type": "script"}, "name": "__run"}
  ]
}
[2026-10-05 05:06:53Z INFO Worker] later line {`)
		require.NoError(t, err)
		assert.Equal(t, []JobStep{
			{Type: "repository", RepositoryType: "GitHub", Name: "dattathallam/curate-pre-probe", Ref: "v2"},
			{Type: "repository", RepositoryType: "GitHub", Name: "actions/cache", Ref: "v4", Path: "restore"},
			{Type: "containerRegistry"},
			{Type: "script"},
		}, facts.Steps)
		assert.Equal(t, "32ef2141-1ecb-4383-a4cf-d6d9e08a9de6", facts.TimelineID)
		assert.False(t, facts.HasServices)
		assert.False(t, facts.HasContainer)
	})
	t.Run("verify when a step is a local action then it carries no name or ref", func(t *testing.T) {
		facts, err := parseJobMessage(jobMessageLine + `{"steps": [{"reference": {"type": "repository", "repositoryType": "self", "path": "./.github/actions/x"}}]}`)
		require.NoError(t, err)
		assert.Equal(t, []JobStep{{Type: "repository", RepositoryType: "self", Path: "./.github/actions/x"}}, facts.Steps)
	})
	t.Run("verify when a script repeats the marker and holds braces then the first message is read whole", func(t *testing.T) {
		facts, err := parseJobMessage(jobMessageLine + `{"steps": [{"reference": {"type": "script"}, "inputs": "echo 'Worker] Job message:' } {{ \"}\""},
 {"reference": {"type": "repository", "name": "a/b", "ref": "v1", "repositoryType": "GitHub"}}]}`)
		require.NoError(t, err)
		require.Len(t, facts.Steps, 2)
		assert.Equal(t, "a/b", facts.Steps[1].Name)
	})
	t.Run("verify when the job has a container or services then they are reported, and null or absent means none", func(t *testing.T) {
		facts, err := parseJobMessage(jobMessageLine + `{"jobContainer": {"type": 2, "map": []}, "jobServiceContainers": {"type": 2, "map": [{"Key": "db"}]}}`)
		require.NoError(t, err)
		assert.True(t, facts.HasContainer)
		assert.True(t, facts.HasServices)
		facts, err = parseJobMessage(jobMessageLine + `{"jobContainer": null, "steps": []}`)
		require.NoError(t, err)
		assert.False(t, facts.HasContainer)
		assert.False(t, facts.HasServices)
	})
	t.Run("verify when a step continues on error then a literal true or an expression counts and a literal false or none does not", func(t *testing.T) {
		facts, err := parseJobMessage(jobMessageLine + `{"steps": [
 {"reference": {"type": "script"}, "continueOnError": {"type": 3, "bool": true}},
 {"reference": {"type": "script"}, "continueOnError": {"type": 3, "expr": "matrix.os == 'x'"}},
 {"reference": {"type": "script"}, "continueOnError": {"type": 3, "bool": false}},
 {"reference": {"type": "script"}}]}`)
		require.NoError(t, err)
		var got []bool
		for _, s := range facts.Steps {
			got = append(got, s.ContinueOnError)
		}
		assert.Equal(t, []bool{true, true, false, false}, got)
	})
	t.Run("verify when there is no job message then it fails", func(t *testing.T) {
		for _, content := range []string{"", "[x INFO Worker] Version: 1\n", jobMessageLine + "no json here", jobMessageLine + `{"steps": [`} {
			_, err := parseJobMessage(content)
			assert.Error(t, err, "content %q", content)
		}
	})
}

func TestReadRunnerDiagJobMessage(t *testing.T) {
	const timeline = "32ef2141-1ecb-4383-a4cf-d6d9e08a9de6"
	start := workerStart
	start = start[:len(start)-len("}\n")] + `, "timeline": {"id": "` + timeline + `"}, "jobContainer": {"type": 2}, "steps": [{"reference": {"type": "script"}}]}` + "\n"
	setupLine := func(ref, sha string) string {
		return "Download action repository 'actions/checkout@" + ref + "' (SHA:" + sha + ")\n"
	}
	t.Run("verify when the Worker log is read then the job's facts and the image build are in the snapshot", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"Worker_20261005-050652-utc.log": start + workerLine("2 steps need to build image from 'Dockerfile'"),
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.True(t, snap.JobParsed)
		assert.Equal(t, timeline, snap.Job.TimelineID)
		assert.True(t, snap.Job.HasContainer)
		assert.Len(t, snap.Job.Steps, 1)
		assert.True(t, snap.BuildsImage)
		assert.False(t, snap.Planted)
		assert.False(t, snap.SetupBufferUnfiltered)
	})
	t.Run("verify when the job message cannot be decoded as facts then the run still matches and the job is not parsed", func(t *testing.T) {
		bad := workerStart[:len(workerStart)-len("}\n")] + `, "steps": "oops"}` + "\n"
		dir := writeDiag(t, map[string]string{"Worker_20261005-050652-utc.log": bad})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.False(t, snap.JobParsed)
		assert.Empty(t, snap.Job.Steps)
	})
	t.Run("verify when a newer Worker log without the job message sits next to the real one then the snapshot is planted", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"Worker_20261005-050652-utc.log": start,
			"Worker_20261005-050700-utc.log": "padding\n",
			"Worker_20261005-050701-utc.log": "padding\n",
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.True(t, snap.Planted)
	})
	t.Run("verify when the job's own log is the newest then it is not planted", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"Worker_20261005-044838-utc.log": start,
			"Worker_29991231-235959-utc.log": "future, skipped\n",
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.False(t, snap.Planted)
	})
	t.Run("verify when buffers of other jobs sit in pages and blocks then only this job's timeline is read", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"pages/" + timeline + "_r1.1":                     setupLine("v4", shaV4),
			"blocks/" + strings.ToUpper(timeline) + "_r2.1":   setupLine("v3", shaV3),
			"pages/00000000-0000-0000-0000-000000000000_r.1":  setupLine("v1", "1111111111111111111111111111111111111111"),
			"blocks/00000000-0000-0000-0000-000000000000_r.1": setupLine("v2", "2222222222222222222222222222222222222222"),
			"Worker_20261005-050652-utc.log":                  start,
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.Equal(t, []LoggedAction{
			{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4},
			{Owner: "actions", Repo: "checkout", Ref: "v3", SHA: shaV3},
		}, snap.SetupJob)
		assert.False(t, snap.SetupBufferUnfiltered)
	})
	t.Run("verify when the job message names no timeline then every buffer is read and the snapshot says so", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"pages/anything_r1.1":            setupLine("v4", shaV4),
			"Worker_20261005-050652-utc.log": workerStart,
		})
		snap, err := ReadRunnerDiag(dir, testRun, testNow)
		require.NoError(t, err)
		assert.Len(t, snap.SetupJob, 1)
		assert.True(t, snap.SetupBufferUnfiltered)
	})
}
