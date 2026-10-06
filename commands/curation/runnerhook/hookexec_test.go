package runnerhook

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fakeJfExitEnv       = "RUNNERHOOK_TEST_FAKE_JF_EXIT"
	traceEnv            = "RUNNERHOOK_TEST_TRACE"
	previousHookExitEnv = "RUNNERHOOK_TEST_PREVIOUS_EXIT"
)

// TestMain lets this test binary stand in for the jf a generated hook calls: with fakeJfExitEnv set
// it records how it was called and exits with that code instead of running the tests.
func TestMain(m *testing.M) {
	if code, ok := os.LookupEnv(fakeJfExitEnv); ok {
		os.Exit(fakeJf(code))
	}
	os.Exit(m.Run())
}

func fakeJf(code string) int {
	// The real jf logs to stderr; Windows PowerShell must not take that for a failure.
	fmt.Fprintln(os.Stderr, "fake jf: deciding")
	line := fmt.Sprintf("jf %s home=%s server=%s\n", strings.Join(os.Args[1:], " "),
		os.Getenv("JFROG_CLI_HOME_DIR"), os.Getenv("JFROG_CLI_SERVER_ID"))
	if err := appendLine(os.Getenv(traceEnv), line); err != nil {
		return 99
	}
	exit, err := strconv.Atoi(code)
	if err != nil {
		return 98
	}
	return exit
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line)
	return errors.Join(err, f.Close())
}

// quotedNonASCII holds what a path must survive on its way through the hook: a space, a straight and
// a typographic apostrophe, and a non-ASCII letter, which Windows PowerShell 5.1 reads correctly only
// from a script with a byte order mark.
const quotedNonASCII = "jf's ’bin é"

// fakeJfBinary copies this test binary under a quotedNonASCII path, so the hook's quoting and
// encoding are exercised by a real shell.
func fakeJfBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(self)
	require.NoError(t, err)
	name := "jf"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), quotedNonASCII, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o755))
	return path
}

// quotedRunnerDir is a configured runner directory whose path holds what the runner can still start a
// hook from: a space on Windows, an apostrophe elsewhere (see checkHookPath). The other characters
// are exercised through the jf and JFrog home paths, which only the hook script itself has to quote.
func quotedRunnerDir(t *testing.T) string {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	name := "runner's-dir"
	if runtime.GOOS == "windows" {
		name = "runner dir"
	}
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, flavorFor(runtime.GOOS).configScript), []byte("rem\n"), 0o755))
	return dir
}

// previousHook writes a hook that records that it ran and exits with previousHookExitEnv.
func previousHook(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "previous-hook.ps1")
		require.NoError(t, os.WriteFile(path, []byte(
			"Add-Content -LiteralPath $env:"+traceEnv+" -Value 'previous'\nexit [int]$env:"+previousHookExitEnv+"\n"), 0o644))
		return path
	}
	path := filepath.Join(dir, "previous-hook.sh")
	require.NoError(t, os.WriteFile(path, []byte(
		"echo previous >> \"$"+traceEnv+"\"\nexit \"$"+previousHookExitEnv+"\"\n"), 0o644))
	return path
}

// hookShells are the commands the runner would run this OS's hook with
// (HostContext.GetDefaultShellForScript, ScriptHandlerHelpers' argument formats). On Windows both
// PowerShells are tried: pwsh is preferred, Windows PowerShell 5.1 is the fallback and the stricter one.
func hookShells(t *testing.T) map[string]func(script string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS != "windows" {
		return map[string]func(string) *exec.Cmd{"bash": func(script string) *exec.Cmd {
			return exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", script)
		}}
	}
	shells := map[string]func(string) *exec.Cmd{}
	for _, shell := range []string{"powershell", "pwsh"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		shells[shell] = func(script string) *exec.Cmd {
			return exec.Command(shell, "-command", ". '"+strings.ReplaceAll(script, `"`, `\"`)+"'")
		}
	}
	require.NotEmpty(t, shells, "neither powershell nor pwsh is on PATH")
	return shells
}

// runHook runs cmd and returns its exit code.
func runHook(t *testing.T, cmd *exec.Cmd, env ...string) int {
	t.Helper()
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	t.Logf("hook output:\n%s", out)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	require.NoError(t, err)
	return 0
}

func readTrace(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(strings.ReplaceAll(string(content), "\r\n", "\n")), "\n")
}

// anyFailure is a wanted exit code that only has to be non-zero: bash reports a missing script as
// 127 or 1 depending on its version.
const anyFailure = -1

func TestInstalledHookRunsLikeTheRunner(t *testing.T) {
	jf := fakeJfBinary(t)
	tests := []struct {
		name         string
		jfExit       int
		withPrevious bool
		previousExit int
		wantExit     int
		wantPrevious bool
		// removePrevious deletes the chained hook after install, as when an admin removes it later.
		removePrevious bool
	}{
		{name: "verify when jf approves and nothing is chained then the hook passes", jfExit: 0, wantExit: 0},
		{name: "verify when jf rejects then the hook fails with jf's exit code", jfExit: 3, wantExit: 3},
		{name: "verify when jf approves then the chained hook runs after it and decides the exit code", jfExit: 0, withPrevious: true, previousExit: 5, wantExit: 5, wantPrevious: true},
		{name: "verify when jf rejects then the chained hook does not run", jfExit: 2, withPrevious: true, previousExit: 0, wantExit: 2},
		{name: "verify when the chained hook is gone then the hook fails as the runner would", jfExit: 0, withPrevious: true, removePrevious: true, wantExit: anyFailure},
	}
	for shellName, shell := range hookShells(t) {
		for _, tt := range tests {
			t.Run(shellName+"/"+tt.name, func(t *testing.T) {
				dir := quotedRunnerDir(t)
				var previous string
				if tt.withPrevious {
					previous = previousHook(t, t.TempDir())
					require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(HookEnvVar+"="+previous+"\n"), 0o644))
				}
				home := filepath.Join(t.TempDir(), "admin's ’.jfrog é")
				_, err := Install(InstallOptions{RunnerDir: dir, JfPath: jf, JfrogHomeDir: home, ServerID: "prod"})
				require.NoError(t, err)
				if tt.removePrevious {
					require.NoError(t, os.Remove(previous))
				}
				trace := filepath.Join(t.TempDir(), "trace.log")

				got := runHook(t, shell(filepath.Join(dir, hookFolder, flavorFor(runtime.GOOS).scriptName)),
					fakeJfExitEnv+"="+strconv.Itoa(tt.jfExit), traceEnv+"="+trace, previousHookExitEnv+"="+strconv.Itoa(tt.previousExit))

				if runtime.GOOS == "windows" || tt.wantExit == anyFailure {
					// Under powershell -command an exit inside the dot-sourced hook ends the script only, and
					// the host then reports 1 for any failure. The runner fails the job on any non-zero code.
					assert.Equal(t, tt.wantExit != 0, got != 0, "the hook must fail exactly when jf or the chained hook fails, got exit code %d", got)
				} else {
					assert.Equal(t, tt.wantExit, got, "the hook's exit code is what fails or passes the job")
				}
				want := []string{"jf curate-gh-actions --runner-hook --runner-dir " + dir + " home=" + home + " server=prod"}
				if tt.wantPrevious {
					want = append(want, "previous")
				}
				assert.Equal(t, want, readTrace(t, trace))
			})
		}
	}
}

func TestInstalledHookFailsWhenJfIsMissing(t *testing.T) {
	for shellName, shell := range hookShells(t) {
		t.Run(shellName+"/verify when the jf binary is gone then the hook fails rather than passing the job", func(t *testing.T) {
			dir := fakeRunner(t, "")
			_, err := Install(InstallOptions{RunnerDir: dir, JfPath: filepath.Join(t.TempDir(), "no-such-jf"), JfrogHomeDir: t.TempDir()})
			require.NoError(t, err)
			got := runHook(t, shell(filepath.Join(dir, hookFolder, flavorFor(runtime.GOOS).scriptName)))
			assert.NotEqual(t, 0, got)
		})
	}
}
