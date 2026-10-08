package githubactions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ActionRepositoryEnvVar names the "owner/repo" of the action whose script is running. In a pre
	// it is the runner's own value: a job or step env: cannot override it.
	ActionRepositoryEnvVar = "GITHUB_ACTION_REPOSITORY"
	// RunnerEnvironmentEnvVar is "github-hosted" on a GitHub-hosted runner; like
	// ActionRepositoryEnvVar, the workflow cannot override it for a pre.
	RunnerEnvironmentEnvVar = "RUNNER_ENVIRONMENT"
)

// ErrRunnerNotVisible means the walk up the process tree did not reach the runner's Worker, as in a
// container job, whose processes are started inside the container.
var ErrRunnerNotVisible = errors.New("the runner process is not visible from here (container job or unusual layout)")

// maxProcessHops bounds the walk; on a hosted runner the Worker is two hops up.
const maxProcessHops = 32

// processInfo is one process as the walk sees it.
type processInfo struct {
	PID, ParentPID int
	Exe            string
}

// inspector reads one process.
type inspector func(pid int) (processInfo, error)

// DetectRunnerDir returns the directory of the runner running this process's job, found by walking
// up from this process to the Worker. Nothing it uses comes from the environment, which a workflow
// author controls.
func DetectRunnerDir() (string, error) {
	inspect, err := newInspector()
	if err != nil {
		if errors.Is(err, ErrRunnerNotVisible) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", ErrRunnerNotVisible, err)
	}
	return FindRunnerDir(os.Getpid(), inspect)
}

// FindRunnerDir walks the parent chain from pid until it reaches Runner.Worker and returns the runner
// directory: the parent of the folder that holds the Worker binary ("bin"). Every failure wraps
// ErrRunnerNotVisible, as do DetectRunnerDir's.
func FindRunnerDir(pid int, inspect inspector) (string, error) {
	return findRunnerDir(pid, inspect, isRunnerDir)
}

// findRunnerDir is FindRunnerDir with the directory check passed in, so a test can name hosted paths
// that do not exist on its machine. A process whose exe is not the Worker only leads to its parent.
func findRunnerDir(pid int, inspect inspector, isRunnerDir func(dir string) bool) (string, error) {
	for range maxProcessHops {
		if pid <= 1 {
			break
		}
		p, err := inspect(pid)
		if err != nil {
			return "", fmt.Errorf("%w: reading process %d: %w", ErrRunnerNotVisible, pid, err)
		}
		binDir, name := splitAnyPath(p.Exe)
		if !strings.EqualFold(name, "Runner.Worker") && !strings.EqualFold(name, "Runner.Worker.exe") {
			pid = p.ParentPID
			continue
		}
		runnerDir, bin := splitAnyPath(binDir)
		if runnerDir == "" || !strings.EqualFold(bin, "bin") || !isRunnerDir(runnerDir) {
			return "", fmt.Errorf("%w: the Worker %q is not in the bin folder of a runner directory", ErrRunnerNotVisible, p.Exe)
		}
		return runnerDir, nil
	}
	return "", ErrRunnerNotVisible
}

// splitAnyPath splits p at its last separator, '\' or '/', whatever the OS, so a Windows path from the
// process table splits the same way in a test on Linux, where filepath does not treat '\' as one.
func splitAnyPath(p string) (dir, base string) {
	i := strings.LastIndexAny(p, `\/`)
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// isRunnerDir reports whether dir holds the Worker binary and _diag, so a binary that is merely named
// Runner.Worker does not send the log reading somewhere else.
func isRunnerDir(dir string) bool {
	if info, err := os.Stat(filepath.Join(dir, "_diag")); err != nil || !info.IsDir() {
		return false
	}
	for _, name := range []string{"Runner.Worker", "Runner.Worker.exe"} {
		if info, err := os.Stat(filepath.Join(dir, "bin", name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}
