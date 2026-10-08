package githubactions

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProcesses is a process table keyed by pid; an inspector over it fails for a pid it does not hold.
type fakeProcesses map[int]processInfo

func (f fakeProcesses) inspect(pid int) (processInfo, error) {
	p, ok := f[pid]
	if !ok {
		return processInfo{}, errors.New("no such process")
	}
	return p, nil
}

// preChain is the hosted pre's process tree: jf (100), started by the action's node (90), started by
// the Worker (80), whose own parent is the Listener (70).
func preChain(workerExe string) fakeProcesses {
	return fakeProcesses{
		100: {ParentPID: 90, Exe: "/opt/jf"},
		90:  {ParentPID: 80, Exe: "/opt/node20/bin/node"},
		80:  {ParentPID: 70, Exe: workerExe},
		70:  {ParentPID: 1, Exe: "/opt/runner/bin/Runner.Listener"},
	}
}

func TestFindRunnerDirOnHostedLayouts(t *testing.T) {
	tests := []struct {
		name      string
		workerExe string
		want      string
	}{
		{
			name:      "verify when the Worker runs from the Linux hosted runner then its versioned folder is the runner dir",
			workerExe: "/home/runner/actions-runner/cached/2.337.0/bin/Runner.Worker",
			want:      "/home/runner/actions-runner/cached/2.337.0",
		},
		{
			name:      "verify when the Worker runs from the macOS hosted runner then the extracted folder is the runner dir",
			workerExe: "/Users/runner/actions-runner/extracted/bin/Runner.Worker",
			want:      "/Users/runner/actions-runner/extracted",
		},
		{
			name:      "verify when the Worker runs from the Windows hosted runner then the path is split on backslashes on any OS",
			workerExe: `C:\actions-runner\cached\2.337.0\bin\Runner.Worker.exe`,
			want:      `C:\actions-runner\cached\2.337.0`,
		},
		{
			name:      "verify when Windows reports the binary in another case then it is still the Worker",
			workerExe: `C:\actions-runner\cached\2.337.0\BIN\runner.worker.EXE`,
			want:      `C:\actions-runner\cached\2.337.0`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var checked []string
			got, err := findRunnerDir(100, preChain(tt.workerExe).inspect, func(dir string) bool {
				checked = append(checked, dir)
				return true
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, []string{tt.want}, checked, "only the Worker's runner dir is checked")
		})
	}
}

func TestFindRunnerDirNotVisible(t *testing.T) {
	accept := func(string) bool { return true }
	t.Run("verify when the chain reaches pid 1 without a Worker then the runner is not visible", func(t *testing.T) {
		// A container job: the action's node is started by the container's init, not by the Worker.
		chain := fakeProcesses{
			100: {ParentPID: 90, Exe: "/usr/local/bin/jf"},
			90:  {ParentPID: 1, Exe: "/__e/node20/bin/node"},
		}
		_, err := findRunnerDir(100, chain.inspect, accept)
		assert.ErrorIs(t, err, ErrRunnerNotVisible)
	})
	t.Run("verify when a process of the chain cannot be read then the runner is not visible", func(t *testing.T) {
		_, err := findRunnerDir(100, fakeProcesses{100: {ParentPID: 90, Exe: "/opt/jf"}}.inspect, accept)
		assert.ErrorIs(t, err, ErrRunnerNotVisible)
	})
	t.Run("verify when the chain loops then the walk gives up", func(t *testing.T) {
		chain := fakeProcesses{
			100: {ParentPID: 90, Exe: "/opt/jf"},
			90:  {ParentPID: 100, Exe: "/opt/node"},
		}
		_, err := findRunnerDir(100, chain.inspect, accept)
		assert.ErrorIs(t, err, ErrRunnerNotVisible)
	})
	t.Run("verify when the Worker's folder is not a runner directory then the runner is not visible", func(t *testing.T) {
		_, err := findRunnerDir(100, preChain("/home/runner/actions-runner/cached/2.337.0/bin/Runner.Worker").inspect,
			func(string) bool { return false })
		assert.ErrorIs(t, err, ErrRunnerNotVisible)
	})
	t.Run("verify when the Worker binary is not in a bin folder then the runner is not visible", func(t *testing.T) {
		_, err := findRunnerDir(100, preChain("/home/runner/Runner.Worker").inspect, accept)
		assert.ErrorIs(t, err, ErrRunnerNotVisible)
	})
}

// workerLayout creates a runner directory with the Worker binary and, when withDiag is set, _diag.
func workerLayout(t *testing.T, withDiag bool) (runnerDir, workerExe string) {
	t.Helper()
	runnerDir = t.TempDir()
	workerExe = filepath.Join(runnerDir, "bin", "Runner.Worker")
	require.NoError(t, os.MkdirAll(filepath.Dir(workerExe), 0o755))
	require.NoError(t, os.WriteFile(workerExe, nil, 0o755))
	if withDiag {
		require.NoError(t, os.MkdirAll(filepath.Join(runnerDir, "_diag"), 0o755))
	}
	return runnerDir, workerExe
}

func TestFindRunnerDirChecksTheDirectory(t *testing.T) {
	t.Run("verify when the runner dir holds the Worker binary and _diag then it is returned", func(t *testing.T) {
		runnerDir, workerExe := workerLayout(t, true)
		got, err := findRunnerDir(100, preChain(workerExe).inspect, isRunnerDir)
		require.NoError(t, err)
		assert.Equal(t, runnerDir, got)
	})
	t.Run("verify when the runner dir has no _diag then it is refused", func(t *testing.T) {
		_, workerExe := workerLayout(t, false)
		_, err := findRunnerDir(100, preChain(workerExe).inspect, isRunnerDir)
		assert.ErrorIs(t, err, ErrRunnerNotVisible)
	})
}

func TestInspectorReadsThisProcess(t *testing.T) {
	inspect, err := newInspector()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		assert.ErrorIs(t, err, ErrRunnerNotVisible)
		return
	}
	require.NoError(t, err)
	got, err := inspect(os.Getpid())
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got.Exe), "inspect(self).Exe = %q, want an absolute path", got.Exe)
	assert.Equal(t, os.Getppid(), got.ParentPID)
	assert.True(t, strings.EqualFold(filepath.Base(exe), filepath.Base(got.Exe)), "inspect(self).Exe = %q, want the test binary %q", got.Exe, exe)
	_, err = inspect(1 << 30)
	assert.Error(t, err, "a pid no process has must not read as a process")
}
