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
	const timeline = "32ef2141-1ecb-4383-a4cf-d6d9e08a9de6"
	// withJob is workerStart whose job message also names a timeline, a job container and one script step.
	withJob := workerStart[:len(workerStart)-len("}\n")] +
		`, "timeline": {"id": "` + timeline + `"}, "jobContainer": {"type": 2}, "steps": [{"reference": {"type": "script"}}]}` + "\n"
	withJobFacts := JobFacts{Steps: []JobStep{{Type: "script"}}, HasContainer: true, TimelineID: timeline}
	download := func(sha string) string {
		return workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + sha + "' into x")
	}
	setupLine := func(ref, sha string) string {
		return "Download action repository 'actions/checkout@" + ref + "' (SHA:" + sha + ")\n"
	}
	savedV4 := []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceSave}}
	loggedV4 := []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}}

	tests := []struct {
		name  string
		files map[string]string // _diag-relative name -> content; nil means there is no _diag folder
		want  DiagSnapshot      // compared whole when the read succeeds
		// wantErr is a substring the error must contain; "" means the read succeeds.
		wantErr string
	}{
		{
			name: "verify when the job message names no timeline then every buffer file and the Worker log are read and the buffer is marked unfiltered",
			files: map[string]string{
				"pages/a_1.log":                  setupLine("v4", shaV4),
				"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
			},
			want: DiagSnapshot{SetupJob: loggedV4, Worker: savedV4, WorkerFiles: []string{"Worker_20261005-050652-utc.log"},
				JobParsed: true, SetupBufferUnfiltered: true},
		},
		{
			name:  "verify when the setup buffer is already gone then the Worker log is still read",
			files: map[string]string{"Worker_20261005-050652-utc.log": workerStart + download(shaV4)},
			want: DiagSnapshot{Worker: savedV4, WorkerFiles: []string{"Worker_20261005-050652-utc.log"},
				JobParsed: true, SetupBufferUnfiltered: true},
		},
		{
			name: "verify when part of the setup buffer cannot be listed then the rest and the Worker log are still read",
			files: map[string]string{
				"pages":                          "a file where the runner keeps a folder, so it cannot be listed",
				"blocks/b_1.log":                 setupLine("v4", shaV4),
				"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
			},
			want: DiagSnapshot{SetupJob: loggedV4, Worker: savedV4, WorkerFiles: []string{"Worker_20261005-050652-utc.log"},
				JobParsed: true, SetupBufferUnfiltered: true},
		},
		{
			// A rollover and a planted file cannot be told apart, so a newer file without the job message
			// is read and the snapshot is marked planted; the caller then verifies by content.
			name: "verify when a newer Worker log without the job message follows the job's own then both are read and the snapshot is planted",
			files: map[string]string{
				"Worker_20261005-044838-utc.log": workerStart + download(shaV3), // an earlier job: must not be read
				"Worker_20261005-050652-utc.log": workerStart + "padding\n",     // this job's first file
				"Worker_20261005-051117-utc.log": download(shaV4),               // rolled or planted, no start marker
			},
			want: DiagSnapshot{Worker: savedV4, WorkerFiles: []string{"Worker_20261005-050652-utc.log", "Worker_20261005-051117-utc.log"},
				JobParsed: true, Planted: true, SetupBufferUnfiltered: true},
		},
		{
			// The runner names a log when its Worker starts, so a later name is one a job planted to sort last.
			name: "verify when a Worker log is dated after now then it is not read and the snapshot is not planted",
			files: map[string]string{
				"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
				"Worker_29991231-235959-utc.log": workerStart + download(shaV3),
			},
			want: DiagSnapshot{Worker: savedV4, WorkerFiles: []string{"Worker_20261005-050652-utc.log"},
				JobParsed: true, SetupBufferUnfiltered: true},
		},
		{
			name:    "verify when no Worker log starts a process then it fails",
			files:   map[string]string{"Worker_20261005-051117-utc.log": download(shaV4)},
			wantErr: "Worker] Version:",
		},
		{
			name:    "verify when there is no _diag folder then it fails",
			wantErr: "Worker] Version:",
		},
		{
			name:    "verify when the newest Worker log names another run then it fails naming the file",
			files:   map[string]string{"Worker_20261005-050652-utc.log": workerStartFor(RunIdentity{RunID: "1", RunAttempt: "1", Repository: testRun.Repository}) + download(shaV4)},
			wantErr: "Worker_20261005-050652-utc.log",
		},
		{
			name:  "verify when the job message names steps, a container and a timeline and the log builds an image then the snapshot carries them",
			files: map[string]string{"Worker_20261005-050652-utc.log": withJob + workerLine("2 steps need to build image from 'Dockerfile'")},
			want: DiagSnapshot{WorkerFiles: []string{"Worker_20261005-050652-utc.log"}, Job: withJobFacts, JobParsed: true,
				BuildsImage: true},
		},
		{
			name:  "verify when the job message cannot be decoded as facts then the run still matches and the job is not parsed",
			files: map[string]string{"Worker_20261005-050652-utc.log": workerStart[:len(workerStart)-len("}\n")] + `, "steps": "oops"}` + "\n"},
			want:  DiagSnapshot{WorkerFiles: []string{"Worker_20261005-050652-utc.log"}, SetupBufferUnfiltered: true},
		},
		{
			name: "verify when buffers of other jobs sit in pages and blocks then only this job's timeline is read",
			files: map[string]string{
				"pages/" + timeline + "_r1.1":                     setupLine("v4", shaV4),
				"blocks/" + strings.ToUpper(timeline) + "_r2.1":   setupLine("v3", shaV3),
				"pages/00000000-0000-0000-0000-000000000000_r.1":  setupLine("v1", "1111111111111111111111111111111111111111"),
				"blocks/00000000-0000-0000-0000-000000000000_r.1": setupLine("v2", "2222222222222222222222222222222222222222"),
				"Worker_20261005-050652-utc.log":                  withJob,
			},
			want: DiagSnapshot{
				SetupJob:    []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}, {Owner: "actions", Repo: "checkout", Ref: "v3", SHA: shaV3}},
				WorkerFiles: []string{"Worker_20261005-050652-utc.log"}, Job: withJobFacts, JobParsed: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadRunnerDiag(writeDiag(t, tt.files), testRun, testNow)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr, "ReadRunnerDiag() = %+v", got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got, "ReadRunnerDiag()")
		})
	}
}

func TestReadRunnerDiagRefusesARunItCannotName(t *testing.T) {
	// Without the run the job belongs to, a Worker log cannot be told from one a job planted.
	dir := writeDiag(t, map[string]string{"Worker_20261005-050652-utc.log": workerStart})

	_, err := ReadRunnerDiag(dir, RunIdentity{}, testNow)

	assert.ErrorContains(t, err, "GITHUB_RUN_ID")
}

const jobMessageLine = "[2026-10-05 05:06:52Z INFO Worker] Job message:\n "

func TestParseJobMessage(t *testing.T) {
	tests := []struct {
		name    string
		content string // Worker log text holding the job message
		want    JobFacts
		wantErr bool
	}{
		{
			name: "verify when the job has four kinds of step then they are read in order with the timeline",
			content: jobMessageLine + `{
  "timeline": {"id": "32ef2141-1ecb-4383-a4cf-d6d9e08a9de6", "changeId": 0, "location": null},
  "steps": [
    {"type": "action", "reference": {"type": "repository", "name": "dattathallam/curate-pre-probe", "ref": "v2", "repositoryType": "GitHub"}, "name": "__curate"},
    {"type": "action", "reference": {"type": "repository", "name": "actions/cache", "ref": "v4", "path": "restore", "repositoryType": "GitHub"}},
    {"type": "action", "reference": {"type": "containerRegistry", "image": "alpine:3.20"}, "name": "__alpine_3_20"},
    {"type": "action", "reference": {"type": "script"}, "name": "__run"}
  ]
}
[2026-10-05 05:06:53Z INFO Worker] later line {`,
			want: JobFacts{
				Steps: []JobStep{
					{Type: "repository", RepositoryType: "GitHub", Name: "dattathallam/curate-pre-probe", Ref: "v2"},
					{Type: "repository", RepositoryType: "GitHub", Name: "actions/cache", Ref: "v4", Path: "restore"},
					{Type: "containerRegistry"},
					{Type: "script"},
				},
				TimelineID: "32ef2141-1ecb-4383-a4cf-d6d9e08a9de6",
			},
		},
		{
			name:    "verify when a step is a local action then it carries no name or ref",
			content: jobMessageLine + `{"steps": [{"reference": {"type": "repository", "repositoryType": "self", "path": "./.github/actions/x"}}]}`,
			want:    JobFacts{Steps: []JobStep{{Type: "repository", RepositoryType: "self", Path: "./.github/actions/x"}}},
		},
		{
			name: "verify when a script repeats the marker and holds braces then the first message is read whole",
			content: jobMessageLine + `{"steps": [{"reference": {"type": "script"}, "inputs": "echo 'Worker] Job message:' } {{ \"}\""},
 {"reference": {"type": "repository", "name": "a/b", "ref": "v1", "repositoryType": "GitHub"}}]}`,
			want: JobFacts{Steps: []JobStep{{Type: "script"}, {Type: "repository", RepositoryType: "GitHub", Name: "a/b", Ref: "v1"}}},
		},
		{
			name:    "verify when the job has a container and services then both are reported",
			content: jobMessageLine + `{"jobContainer": {"type": 2, "map": []}, "jobServiceContainers": {"type": 2, "map": [{"Key": "db"}]}}`,
			want:    JobFacts{HasContainer: true, HasServices: true},
		},
		{
			name:    "verify when the container is null and services are absent then neither is reported",
			content: jobMessageLine + `{"jobContainer": null, "steps": []}`,
			want:    JobFacts{},
		},
		{
			name: "verify when a step continues on error then a literal true or an expression counts and a literal false or none does not",
			content: jobMessageLine + `{"steps": [
 {"reference": {"type": "script"}, "continueOnError": {"type": 3, "bool": true}},
 {"reference": {"type": "script"}, "continueOnError": {"type": 3, "expr": "matrix.os == 'x'"}},
 {"reference": {"type": "script"}, "continueOnError": {"type": 3, "bool": false}},
 {"reference": {"type": "script"}}]}`,
			want: JobFacts{Steps: []JobStep{
				{Type: "script", ContinueOnError: true},
				{Type: "script", ContinueOnError: true},
				{Type: "script"},
				{Type: "script"},
			}},
		},
		{
			name:    "verify when the log has no job message marker then it fails",
			content: "[x INFO Worker] Version: 1\n",
			wantErr: true,
		},
		{
			name:    "verify when the job message holds no JSON then it fails",
			content: jobMessageLine + "no json here",
			wantErr: true,
		},
		{
			name:    "verify when the job message JSON is cut short then it fails",
			content: jobMessageLine + `{"steps": [`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJobMessage(tt.content)

			if tt.wantErr {
				assert.Error(t, err, "parseJobMessage(%q) = %+v", tt.content, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got, "parseJobMessage()")
		})
	}
}

func TestJobFactsFirstStepContinuesOnError(t *testing.T) {
	ourStep := func(name string, coe bool) JobStep {
		return JobStep{Type: "repository", RepositoryType: "GitHub", Name: name, ContinueOnError: coe}
	}
	tests := []struct {
		name          string
		steps         []JobStep
		wantContinues bool
		wantFound     bool
	}{
		{name: "verify when the first step is ours without continue-on-error then the failure stands", steps: []JobStep{ourStep("jfrog/curate", false)}, wantFound: true},
		{name: "verify when the first step is ours with continue-on-error then the failure may be swallowed", steps: []JobStep{ourStep("JFrog/Curate", true)}, wantContinues: true, wantFound: true},
		{name: "verify when another step is first then ours is not found", steps: []JobStep{ourStep("actions/checkout", false), ourStep("jfrog/curate", false)}},
		{name: "verify when the job message has no steps then ours is not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			continues, found := JobFacts{Steps: tt.steps}.FirstStepContinuesOnError("jfrog/curate")
			assert.Equal(t, tt.wantContinues, continues, "FirstStepContinuesOnError() continues")
			assert.Equal(t, tt.wantFound, found, "FirstStepContinuesOnError() found")
		})
	}
}
