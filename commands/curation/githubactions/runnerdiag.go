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
}

// ReadRunnerDiag reads runnerDir/_diag. The setup buffer is read first: the runner deletes it a few
// seconds after the job-started hook begins, so nothing slow may run before this.
//
// The Worker logs read are this process's: newest first, back to the one holding workerStartMarker,
// because a large job rolls to a new file at WORKER_LOGSIZE. The runner runs one job at a time, so
// the newest file belongs to the running job - but a job runs as the user that owns _diag, so a
// previous one can plant a log there. A log named after now is skipped, since the runner names each
// when its Worker starts, and the log read must hold the job message for run; otherwise this fails,
// and the caller curates by content rather than by a SHA a job could have chosen.
//
// A part of the setup buffer that cannot be read is skipped rather than failing the read: the Worker
// log does not depend on it, and every setup line is later checked against the Worker log, so a
// missing one costs at most the fast path.
func ReadRunnerDiag(runnerDir string, run RunIdentity, now time.Time) (DiagSnapshot, error) {
	diag := filepath.Join(runnerDir, "_diag")
	var setup strings.Builder
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
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(diag, sub, entry.Name())
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
	found := false
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return DiagSnapshot{}, errorutils.CheckError(err)
		}
		picked = append(picked, filepath.Base(file))
		contents = append(contents, string(content))
		if strings.Contains(string(content), workerStartMarker) {
			if logged := jobMessageRun(string(content)); !logged.sameRun(run) {
				return DiagSnapshot{}, errorutils.CheckErrorf("the newest Worker log, %q, is not this job's: it names run %q attempt %q of %q, "+
					"not run %q attempt %q of %q", file, logged.RunID, logged.RunAttempt, logged.Repository, run.RunID, run.RunAttempt, run.Repository)
			}
			found = true
			break
		}
	}
	if !found {
		return DiagSnapshot{}, errorutils.CheckError(fmt.Errorf("no Worker log in %q contains %q, so this job's log cannot be read", diag, workerStartMarker))
	}
	slices.Reverse(picked)
	slices.Reverse(contents)
	return DiagSnapshot{
		SetupJob:    ParseSetupJobLines(setup.String()),
		Worker:      ParseWorkerLog(strings.Join(contents, "\n")),
		WorkerFiles: picked,
	}, nil
}

// workerLogNameLayout is the runner's Worker log name (HostTraceListener, "{0}_{1:yyyyMMdd-HHmmss}-utc.log").
const workerLogNameLayout = "Worker_20060102-150405-utc.log"

// jobMessageMarker opens the job message a Worker logs, as indented JSON, right after it starts (Worker.cs).
const jobMessageMarker = "Worker] Job message:"

// jobMessageRun is the run the Worker log's job message names, read from its github context; empty
// fields when the log holds no job message or the message does not name them.
func jobMessageRun(content string) RunIdentity {
	_, message, ok := strings.Cut(content, jobMessageMarker)
	start := strings.IndexByte(message, '{')
	if !ok || start < 0 {
		return RunIdentity{}
	}
	// One JSON value is decoded; the log lines after it are left unread. A context dictionary is
	// {"t": 2, "d": [{"k": <name>, "v": <value>}, ...]} (PipelineContextDataJsonConverter).
	var job struct {
		ContextData map[string]struct {
			D []struct {
				K string
				V json.RawMessage
			}
		}
	}
	if err := json.NewDecoder(strings.NewReader(message[start:])).Decode(&job); err != nil {
		return RunIdentity{}
	}
	var run RunIdentity
	for _, entry := range job.ContextData["github"].D {
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
