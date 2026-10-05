package githubactions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

// workerStartMarker is the line every Worker process writes first. A file without it is a
// continuation the log rolled over into.
const workerStartMarker = "Worker] Version:"

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
// the newest file belongs to the running job.
func ReadRunnerDiag(runnerDir string) (DiagSnapshot, error) {
	diag := filepath.Join(runnerDir, "_diag")
	var setup strings.Builder
	for _, sub := range []string{"pages", "blocks"} {
		entries, err := os.ReadDir(filepath.Join(diag, sub))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return DiagSnapshot{}, errorutils.CheckError(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			content, err := os.ReadFile(filepath.Join(diag, sub, entry.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue // uploaded and deleted between the listing and the read
			}
			if err != nil {
				return DiagSnapshot{}, errorutils.CheckError(err)
			}
			setup.Write(content)
			setup.WriteByte('\n')
		}
	}

	files, err := filepath.Glob(filepath.Join(diag, "Worker_*.log"))
	if err != nil {
		return DiagSnapshot{}, errorutils.CheckError(err)
	}
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
