package githubactions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

// workerStartMarker is the line every Worker process writes first. A file without it is a
// continuation the log rolled over into.
const workerStartMarker = "Worker] Version:"

const (
	// RunIDEnvVar and RunAttemptEnvVar name the run the job belongs to; the runner gives them to the hook.
	RunIDEnvVar      = "GITHUB_RUN_ID"
	RunAttemptEnvVar = "GITHUB_RUN_ATTEMPT"
)

// RunIdentity names the workflow run a job belongs to, as the runner gives it to the job-started
// hook and writes it into the job message at the top of the job's Worker log.
type RunIdentity struct {
	RunID, RunAttempt, Repository string
}

// RunIdentityFromEnv is the run the runner says this process belongs to.
func RunIdentityFromEnv() RunIdentity {
	return RunIdentity{RunID: os.Getenv(RunIDEnvVar), RunAttempt: os.Getenv(RunAttemptEnvVar), Repository: os.Getenv(GithubRepoEnvVar)}
}

// DiagSnapshot is what the runner's _diag folder says about this job's actions.
type DiagSnapshot struct {
	// SetupJob is from the "Set up job" output, buffered in _diag/pages and _diag/blocks until
	// uploaded; empty once the runner has uploaded and deleted it.
	SetupJob []LoggedAction
	// Worker is every action this job's Worker logs show the runner materializing.
	Worker []WorkerAction
	// WorkerFiles names the Worker logs read, oldest first.
	WorkerFiles []string
	// Job is what the job message in the Worker log says about the job; zero unless JobParsed.
	Job JobFacts
	// JobParsed is true when the job message was read into Job.
	JobParsed bool
	// Planted is true when Worker logs newer than the one holding the job message were read: the
	// runner starts one Worker per job, so a newer file without a job message is either a rollover
	// of this job's log or a file a job planted, and the two cannot be told apart. PlantedFiles
	// names those newer files.
	Planted      bool
	PlantedFiles []string
	// SetupBufferUnfiltered is true when the setup buffer could not be limited to this job's
	// timeline because the job message names none, so it may hold another job's lines.
	SetupBufferUnfiltered bool
	// BuildsImage is true when the Worker log shows an image built from a Dockerfile, which runs
	// author code before any pre step.
	BuildsImage bool
}

// ReadRunnerDiag reads runnerDir/_diag. The runner deletes the setup buffer (_diag/pages and
// _diag/blocks) a few seconds after the job-started hook begins, so its files are listed first, before
// anything slow, and read once the Worker log has said which timeline is this job's.
//
// The Worker logs read are this process's: newest first, back to the one holding workerStartMarker,
// because a large job rolls to a new file at WORKER_LOGSIZE. The runner runs one job at a time, so
// the newest file belongs to the running job - but a job runs as the user that owns _diag, so a
// previous one can plant a log there. A log named after now is skipped, since the runner names each
// when its Worker starts; one newer than the log holding the job message is reported as Planted; and
// the log read must hold the job message for run; otherwise this fails, and the caller curates by
// content rather than by a SHA a job could have chosen.
//
// A part of the setup buffer that cannot be read is skipped rather than failing the read: the Worker
// log does not depend on it, and every setup line is later checked against the Worker log, so a
// missing one costs at most the fast path.
func ReadRunnerDiag(runnerDir string, run RunIdentity, now time.Time) (DiagSnapshot, error) {
	diag := filepath.Join(runnerDir, "_diag")
	var buffer []string
	for _, sub := range []string{"pages", "blocks"} {
		entries, err := os.ReadDir(filepath.Join(diag, sub))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			log.Warn(fmt.Sprintf("Skipping the runner's setup buffer %q, which cannot be read: %v", filepath.Join(diag, sub), err))
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				buffer = append(buffer, filepath.Join(diag, sub, entry.Name()))
			}
		}
	}

	if run.RunID == "" || run.RunAttempt == "" || run.Repository == "" {
		return DiagSnapshot{}, errorutils.CheckErrorf("the runner did not name this job's run (%s, %s, %s), so its Worker log cannot be told from one a job planted",
			RunIDEnvVar, RunAttemptEnvVar, GithubRepoEnvVar)
	}
	files, err := filepath.Glob(filepath.Join(diag, "Worker_*.log"))
	if err != nil {
		return DiagSnapshot{}, errorutils.CheckError(err)
	}
	files = slices.DeleteFunc(files, func(file string) bool {
		started, err := time.Parse(workerLogNameLayout, filepath.Base(file))
		if err == nil && !started.After(now) {
			return false
		}
		log.Warn(fmt.Sprintf("Ignoring %q: it is not named for a time the runner started a Worker, so a job may have planted it", file))
		return true
	})
	// The names carry a UTC timestamp (Worker_yyyyMMdd-HHmmss-utc.log), so name order is time order.
	slices.Sort(files)
	slices.Reverse(files)
	var picked []string
	var contents []string
	var snap DiagSnapshot
	found := false
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return DiagSnapshot{}, errorutils.CheckError(err)
		}
		picked = append(picked, filepath.Base(file))
		contents = append(contents, string(content))
		if !strings.Contains(string(content), workerStartMarker) {
			continue
		}
		message, err := decodeJobMessage(string(content))
		if err != nil || !message.run().sameRun(run) {
			logged := message.run()
			return DiagSnapshot{}, errorutils.CheckErrorf("the newest Worker log, %q, is not this job's: it names run %q attempt %q of %q, "+
				"not run %q attempt %q of %q", file, logged.RunID, logged.RunAttempt, logged.Repository, run.RunID, run.RunAttempt, run.Repository)
		}
		if snap.Job, err = message.facts(); err != nil {
			log.Debug("The Worker log's job message could not be read into the job's steps; the job is curated by content")
			snap.Job = JobFacts{}
		} else {
			snap.JobParsed = true
		}
		found = true
		break
	}
	if !found {
		return DiagSnapshot{}, errorutils.CheckError(fmt.Errorf("no Worker log in %q contains %q, so this job's log cannot be read", diag, workerStartMarker))
	}
	if len(picked) > 1 {
		snap.Planted = true
		snap.PlantedFiles = slices.Clone(picked[:len(picked)-1])
	}
	slices.Reverse(picked)
	slices.Reverse(contents)
	snap.WorkerFiles = picked
	workerText := strings.Join(contents, "\n")
	snap.Worker = ParseWorkerLog(workerText)
	snap.BuildsImage = WorkerLogBuildsImage(workerText)

	// A buffer file is named <timeline id>_<record id>.<n>; sibling jobs of one run share the timeline.
	prefix := ""
	if snap.Job.TimelineID != "" {
		prefix = strings.ToLower(snap.Job.TimelineID) + "_"
	} else {
		snap.SetupBufferUnfiltered = true
	}
	var setup strings.Builder
	for _, path := range buffer {
		if !strings.HasPrefix(strings.ToLower(filepath.Base(path)), prefix) {
			continue
		}
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // uploaded and deleted between the listing and the read
		}
		if err != nil {
			log.Warn(fmt.Sprintf("Skipping the runner's setup buffer %q, which cannot be read: %v", path, err))
			continue
		}
		setup.Write(content)
		setup.WriteByte('\n')
	}
	snap.SetupJob = ParseSetupJobLines(setup.String())
	return snap, nil
}

// workerLogNameLayout is the runner's Worker log name (HostTraceListener, "{0}_{1:yyyyMMdd-HHmmss}-utc.log").
const workerLogNameLayout = "Worker_20060102-150405-utc.log"

// jobMessageMarker opens the job message a Worker logs, as indented JSON, right after it starts (Worker.cs).
const jobMessageMarker = "Worker] Job message:"

// JobStep is one top-level step of the job, as the job message names it.
type JobStep struct {
	Type string // "repository", "containerRegistry" or "script"
	// RepositoryType is "GitHub" for a repository action and "self" for a local one; compare it
	// case-insensitively. It decides what may be exempted from the Worker-log cross-check.
	RepositoryType string
	Name           string // owner/repo for a repository step
	Ref            string
	Path           string
	// ContinueOnError is true when the step's continueOnError token is the literal true or an
	// expression, which can evaluate to true.
	ContinueOnError bool
}

// JobFacts is what the job message says about the job that the trust rules need.
type JobFacts struct {
	Steps        []JobStep
	HasServices  bool   // jobServiceContainers is present and non-null
	HasContainer bool   // jobContainer is present and non-null
	TimelineID   string // timeline.id; "" when absent
}

// jobMessage is the job message a Worker logs, decoded once. Fields stay raw so a malformed one costs
// only its own facts, not the run identity. encoding/json matches the field names case-insensitively.
type jobMessage struct {
	ContextData map[string]struct {
		D []struct {
			K string
			V json.RawMessage
		}
	}
	Steps                json.RawMessage
	Timeline             json.RawMessage
	JobContainer         json.RawMessage
	JobServiceContainers json.RawMessage
}

// decodeJobMessage decodes the first job message in content. One JSON value is read from the first
// brace after the first marker, so braces inside strings and log lines after it are of no concern.
func decodeJobMessage(content string) (jobMessage, error) {
	_, message, ok := strings.Cut(content, jobMessageMarker)
	if !ok {
		return jobMessage{}, errors.New("no job message in the Worker log")
	}
	start := strings.IndexByte(message, '{')
	if start < 0 {
		return jobMessage{}, errors.New("the job message holds no JSON")
	}
	var m jobMessage
	if err := json.NewDecoder(strings.NewReader(message[start:])).Decode(&m); err != nil {
		return jobMessage{}, fmt.Errorf("decoding the job message: %w", err)
	}
	return m, nil
}

// ParseJobMessage reads the job's steps, container, services and timeline from the job message in a
// Worker log's text.
func ParseJobMessage(content string) (JobFacts, error) {
	m, err := decodeJobMessage(content)
	if err != nil {
		return JobFacts{}, err
	}
	return m.facts()
}

func (m jobMessage) facts() (JobFacts, error) {
	facts := JobFacts{HasServices: present(m.JobServiceContainers), HasContainer: present(m.JobContainer)}
	if present(m.Timeline) {
		var timeline struct{ ID string }
		if err := json.Unmarshal(m.Timeline, &timeline); err != nil {
			return JobFacts{}, fmt.Errorf("reading the job message's timeline: %w", err)
		}
		facts.TimelineID = timeline.ID
	}
	if !present(m.Steps) {
		return facts, nil
	}
	var steps []struct {
		Reference struct {
			Type, RepositoryType, Name, Ref, Path string
		}
		ContinueOnError json.RawMessage
	}
	if err := json.Unmarshal(m.Steps, &steps); err != nil {
		return JobFacts{}, fmt.Errorf("reading the job message's steps: %w", err)
	}
	for _, s := range steps {
		r := s.Reference
		facts.Steps = append(facts.Steps, JobStep{Type: r.Type, RepositoryType: r.RepositoryType, Name: r.Name, Ref: r.Ref, Path: r.Path,
			ContinueOnError: continuesOnError(s.ContinueOnError)})
	}
	return facts, nil
}

// present reports whether a raw JSON field is there and not null.
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// continuesOnError reads a step's continueOnError template token: {"bool": true} or {"bool": false}
// for a literal, {"expr": "..."} for an expression, absent when unset. Anything that is not a
// literal false counts as true, since an expression can evaluate to true.
func continuesOnError(raw json.RawMessage) bool {
	if !present(raw) {
		return false
	}
	var token struct {
		Bool *bool
		Expr *string
	}
	if err := json.Unmarshal(raw, &token); err != nil {
		var literal bool
		return json.Unmarshal(raw, &literal) != nil || literal
	}
	if token.Bool != nil {
		return *token.Bool
	}
	return true
}

// run is the run the job message names, read from its github context: a context dictionary is
// {"t": 2, "d": [{"k": <name>, "v": <value>}, ...]} (PipelineContextDataJsonConverter). Fields are
// empty when the message does not name them.
func (m jobMessage) run() RunIdentity {
	var run RunIdentity
	for _, entry := range m.ContextData["github"].D {
		var value string
		if json.Unmarshal(entry.V, &value) != nil {
			continue
		}
		switch entry.K {
		case "run_id":
			run.RunID = value
		case "run_attempt":
			run.RunAttempt = value
		case "repository":
			run.Repository = value
		}
	}
	return run
}

// sameRun reports whether r and other name the same attempt of the same run. GitHub matches
// repository names regardless of case.
func (r RunIdentity) sameRun(other RunIdentity) bool {
	return r.RunID != "" && r.RunID == other.RunID && r.RunAttempt == other.RunAttempt && strings.EqualFold(r.Repository, other.Repository)
}
