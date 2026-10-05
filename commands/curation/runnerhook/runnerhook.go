// Package runnerhook installs 'jf curate-gh-actions' as a self-hosted runner's job-started hook.
package runnerhook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

const (
	// HookEnvVar is the runner setting that names the job-started hook. The runner reads it from
	// <runner>/.env at start, so a change takes effect when the runner service restarts.
	HookEnvVar       = "ACTIONS_RUNNER_HOOK_JOB_STARTED"
	hookFolder       = ".jfrog"
	hookScriptName   = "curate-gh-actions-hook.sh"
	previousHookFile = "previous-job-started-hook"
)

// InstallOptions are pinned into the hook script, because the runner service usually runs as a
// different user, with a different home, than the admin who ran 'jf config add'.
type InstallOptions struct {
	RunnerDir    string
	JfPath       string // absolute path of the jf binary the hook runs
	JfrogHomeDir string // the admin's JFROG_CLI_HOME_DIR, holding the server configuration
	ServerID     string // "" to use that configuration's default server
}

// Install writes the hook script and points the runner's .env at it. It returns warnings for
// settings that would let a job running on this runner tamper with the check.
func Install(opts InstallOptions) (warnings []string, err error) {
	if err = checkInstallable(opts.RunnerDir); err != nil {
		return nil, err
	}
	folder := filepath.Join(opts.RunnerDir, hookFolder)
	// #nosec G301 G703 -- the runner service, often another user, must read and run what is in here; the
	// runner directory comes from the admin running install, not from a job
	if err = os.MkdirAll(folder, 0o755); err != nil {
		return nil, errorutils.CheckError(err)
	}
	envPath := filepath.Join(opts.RunnerDir, ".env")
	lines, previous, err := readEnv(envPath)
	if err != nil {
		return nil, err
	}
	script := filepath.Join(folder, hookScriptName)
	if previous == script {
		// Already installed: keep whatever was chained before the first install.
		if content, readErr := os.ReadFile(filepath.Join(folder, previousHookFile)); readErr == nil {
			previous = string(content)
		} else {
			previous = ""
		}
	}
	if previous != "" {
		// #nosec G306 G703 -- holds a path, not a secret; read back by uninstall
		if err = os.WriteFile(filepath.Join(folder, previousHookFile), []byte(previous), 0o644); err != nil {
			return nil, errorutils.CheckError(err)
		}
	}
	// #nosec G306 -- the runner service, often another user, must read and run the hook script
	if err = os.WriteFile(script, []byte(hookScript(opts, previous)), 0o755); err != nil {
		return nil, errorutils.CheckError(err)
	}
	if err = writeEnv(envPath, lines, script); err != nil {
		return nil, err
	}
	for _, path := range []string{folder, script, opts.JfPath} {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o022 != 0 {
			warnings = append(warnings, fmt.Sprintf("%s is writable by group or others; a job on this runner could change "+
				"what the hook runs - make it writable by the admin only", path))
		}
	}
	return warnings, nil
}

// Uninstall restores the hook that was configured before Install, or removes the setting.
func Uninstall(runnerDir string) error {
	folder := filepath.Join(runnerDir, hookFolder)
	envPath := filepath.Join(runnerDir, ".env")
	lines, _, err := readEnv(envPath)
	if err != nil {
		return err
	}
	previous := ""
	if content, readErr := os.ReadFile(filepath.Join(folder, previousHookFile)); readErr == nil {
		previous = string(content)
	}
	if err = writeEnv(envPath, lines, previous); err != nil {
		return err
	}
	return errorutils.CheckError(os.RemoveAll(folder))
}

func checkInstallable(runnerDir string) error {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		return errorutils.CheckErrorf("installing the runner hook is a runner-admin operation and cannot run inside a GitHub Actions job")
	}
	if runtime.GOOS == "windows" {
		return errorutils.CheckErrorf("installing the runner hook is not supported on Windows yet")
	}
	for _, name := range []string{".runner", "config.sh"} {
		if _, err := os.Stat(filepath.Join(runnerDir, name)); err != nil {
			return errorutils.CheckErrorf("%q is not a configured GitHub Actions runner directory: %s is missing", runnerDir, name)
		}
	}
	return nil
}

// readEnv returns .env's lines without the hook setting, and the hook it named ("" when none).
func readEnv(path string) (lines []string, hook string, err error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", errorutils.CheckError(err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
		if value, ok := strings.CutPrefix(line, HookEnvVar+"="); ok {
			hook = value
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, hook, nil
}

func writeEnv(path string, lines []string, hook string) error {
	if hook != "" {
		lines = append(lines, HookEnvVar+"="+hook)
	}
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	// #nosec G306 G703 -- the runner's own .env, which the runner service must read; an existing file keeps its mode
	return errorutils.CheckError(os.WriteFile(path, []byte(content), 0o644))
}

// hookScript runs the check first, because the runner's logs it reads are deleted seconds after the
// hook starts, then any hook that was configured before. The runner runs it with 'bash -e', so a
// failed check stops here and fails the job.
func hookScript(opts InstallOptions, previous string) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n# Written by 'jf curate-gh-actions --install-runner-hook'. Re-run that command instead of editing this file.\n")
	sb.WriteString("export JFROG_CLI_HOME_DIR=" + shellQuote(opts.JfrogHomeDir) + "\n")
	if opts.ServerID != "" {
		sb.WriteString("export JFROG_CLI_SERVER_ID=" + shellQuote(opts.ServerID) + "\n")
	}
	sb.WriteString(shellQuote(opts.JfPath) + " curate-gh-actions --runner-hook --runner-dir " + shellQuote(opts.RunnerDir) + "\n")
	if previous != "" {
		// The runner runs a .sh hook with bash rather than executing it, so the chained hook need not
		// be executable; run it the same way.
		sb.WriteString("bash -e " + shellQuote(previous) + "\n")
	}
	return sb.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
