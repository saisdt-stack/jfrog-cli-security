// Package runnerhook installs 'jf curate-gh-actions' as a self-hosted runner's job-started hook.
package runnerhook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

const (
	// HookEnvVar is the runner setting that names the job-started hook. The runner reads it from
	// <runner>/.env at start, so a change takes effect when the runner service restarts.
	HookEnvVar       = "ACTIONS_RUNNER_HOOK_JOB_STARTED"
	hookFolder       = ".jfrog"
	previousHookFile = "previous-job-started-hook"
	scriptHeader     = "# Written by 'jf curate-gh-actions --install-runner-hook'. Re-run that command instead of editing this file.\n"
	utf8BOM          = "\ufeff"
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
// settings that would let a job running on this runner tamper with the check, or keep it from running.
func Install(opts InstallOptions) (warnings []string, err error) {
	// The runner starts the hook from the job's working directory, not the admin's, so every path
	// written into .env or the script must be absolute.
	for _, path := range []*string{&opts.RunnerDir, &opts.JfPath, &opts.JfrogHomeDir} {
		if *path, err = filepath.Abs(*path); err != nil {
			return nil, errorutils.CheckError(err)
		}
	}
	flavor := flavorFor(runtime.GOOS)
	if err = checkInstallable(opts.RunnerDir, flavor); err != nil {
		return nil, err
	}
	folder := filepath.Join(opts.RunnerDir, hookFolder)
	envPath := filepath.Join(opts.RunnerDir, ".env")
	lines, previous, err := readEnv(envPath)
	if err != nil {
		return nil, err
	}
	script := filepath.Join(folder, flavor.scriptName)
	if samePath(previous, script) {
		// Already installed: keep whatever was chained before the first install.
		if previous, err = readPreviousHook(folder); err != nil {
			return nil, err
		}
		if samePath(previous, script) {
			// Left by an install that took our own script for another hook; chaining it would recurse.
			previous = ""
		}
	}
	// Checked before anything is written, so a refusal leaves the runner as it was.
	if err = checkHookPath(runtime.GOOS, script); err != nil {
		return nil, err
	}
	if err = checkChainable(previous); err != nil {
		return nil, err
	}
	// #nosec G301 G703 -- the runner service, often another user, must read and run what is in here; the
	// runner directory comes from the admin running install, not from a job
	if err = os.MkdirAll(folder, 0o755); err != nil {
		return nil, errorutils.CheckError(err)
	}
	if previous != "" {
		// #nosec G306 G703 -- holds a path, not a secret; read back by uninstall
		if err = os.WriteFile(filepath.Join(folder, previousHookFile), []byte(previous), 0o644); err != nil {
			return nil, errorutils.CheckError(err)
		}
	}
	// #nosec G306 -- the runner service, often another user, must read and run the hook script
	if err = os.WriteFile(script, []byte(flavor.render(opts, previous)), 0o755); err != nil {
		return nil, errorutils.CheckError(err)
	}
	if err = writeEnv(envPath, lines, script); err != nil {
		return nil, err
	}
	return tamperWarnings(runtime.GOOS, opts, folder, script), nil
}

// tamperWarnings names what would let a job on this runner change what the hook runs. Off Windows
// that is any of these paths being writable by group or others. On Windows the file mode says nothing
// about who may write, so the warnings say what the admin has to check instead - including that the
// runner service, which by default runs as NETWORK SERVICE, can read the configuration the hook uses.
func tamperWarnings(goos string, opts InstallOptions, folder, script string) []string {
	if goos == "windows" {
		return []string{
			fmt.Sprintf("file permissions are not checked on Windows: allow only administrators to change %s, %s and %s "+
				"(for example with icacls), since a job on this runner could otherwise change what the hook runs", folder, script, opts.JfPath),
			fmt.Sprintf("the runner service account must be able to read the JFrog CLI configuration in %s: the default "+
				`account, NT AUTHORITY\NETWORK SERVICE, usually cannot read a user profile - run the runner service as a `+
				"dedicated account, or grant that account read access", opts.JfrogHomeDir),
		}
	}
	var warnings []string
	for _, path := range []string{folder, script, opts.JfPath} {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o022 != 0 {
			warnings = append(warnings, fmt.Sprintf("%s is writable by group or others; a job on this runner could change "+
				"what the hook runs - make it writable by the admin only", path))
		}
	}
	return warnings
}

// Uninstall restores the hook that was configured before Install, or removes the setting.
func Uninstall(runnerDir string) error {
	folder := filepath.Join(runnerDir, hookFolder)
	envPath := filepath.Join(runnerDir, ".env")
	lines, _, err := readEnv(envPath)
	if err != nil {
		return err
	}
	previous, err := readPreviousHook(folder)
	if err != nil {
		return err
	}
	if err = writeEnv(envPath, lines, previous); err != nil {
		return err
	}
	return errorutils.CheckError(os.RemoveAll(folder))
}

// readPreviousHook returns the hook that was set before the first install, or "" when there was none.
// Any error other than the file being absent is returned: taking it for "none" would drop the admin's
// hook from the chain on re-install, and from .env on uninstall.
func readPreviousHook(folder string) (string, error) {
	content, err := os.ReadFile(filepath.Join(folder, previousHookFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errorutils.CheckError(fmt.Errorf("reading the hook that ran before curate-gh-actions was installed: %w", err))
	}
	return string(content), nil
}

func checkInstallable(runnerDir string, flavor hookFlavor) error {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		return errorutils.CheckErrorf("installing the runner hook is a runner-admin operation and cannot run inside a GitHub Actions job")
	}
	for _, name := range []string{".runner", flavor.configScript} {
		if _, err := os.Stat(filepath.Join(runnerDir, name)); err != nil {
			return errorutils.CheckErrorf("'%s' is not a configured GitHub Actions runner directory: %s is missing", runnerDir, name)
		}
	}
	return nil
}

// samePath reports whether a and b name the same file. The runner directory may be given under
// another spelling than at the last install - another letter case on Windows and macOS, or a symlink -
// and our own script must never be taken for a hook to chain, or it would run itself on every job.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	// #nosec G703 -- both paths come from the runner's .env and the directory the admin named; they are
	// only stat'ed, never opened
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
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
	// The runner's File.ReadAllLines detects the encoding; this rewrites the file line by line, which is
	// safe only for UTF-8. A UTF-16 file (Out-File in Windows PowerShell) would come out corrupted.
	if bytes.HasPrefix(content, []byte{0xff, 0xfe}) || bytes.HasPrefix(content, []byte{0xfe, 0xff}) || bytes.IndexByte(content, 0) >= 0 {
		return nil, "", errorutils.CheckErrorf("'%s' is not UTF-8 text (it looks like UTF-16): save it as UTF-8 and run the command again", path)
	}
	content = bytes.TrimPrefix(content, []byte(utf8BOM))
	for _, line := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
		// Windows editors save .env with CRLF; the runner's File.ReadAllLines drops the \r, so do the same.
		line = strings.TrimSuffix(line, "\r")
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

// hookFlavor is how the hook is written for one OS. The runner picks a hook's interpreter by its
// extension - bash for .sh, pwsh or powershell for .ps1 (HostContext.GetDefaultShellForScript) - and
// a Windows runner cannot be relied on to have bash.
type hookFlavor struct {
	scriptName   string
	configScript string // the runner's configure script, which every runner directory holds
	render       func(opts InstallOptions, previous string) string
}

func flavorFor(goos string) hookFlavor {
	if goos == "windows" {
		return hookFlavor{scriptName: "curate-gh-actions-hook.ps1", configScript: "config.cmd", render: powerShellHookScript}
	}
	return hookFlavor{scriptName: "curate-gh-actions-hook.sh", configScript: "config.sh", render: bashHookScript}
}

// checkHookPath refuses a hook path the runner could not start. The runner starts a hook with one
// argument string (ProcessInvoker sets StartInfo.Arguments), escaping only double quotes in the path:
// a .ps1 path goes inside single quotes (powershell -command ". '<path>'"), so a single quote - PowerShell
// takes the typographic ones as single quotes too - ends it early; a .sh path goes in unquoted (bash
// -e <path>), so whitespace splits it. Either way every job would fail before its first step.
func checkHookPath(goos, script string) error {
	if goos == "windows" {
		if strings.ContainsAny(script, powerShellQuotes) {
			return errorutils.CheckErrorf("the runner cannot start a hook at '%s': it passes the hook's path to PowerShell inside "+
				"single quotes without escaping them - install the runner in a directory whose path has no quote characters", script)
		}
		return nil
	}
	if strings.ContainsFunc(script, unicode.IsSpace) {
		return errorutils.CheckErrorf("the runner cannot start a hook at '%s': it passes the hook's path to bash unquoted, so "+
			"whitespace splits it - install the runner in a directory whose path has no spaces", script)
	}
	return nil
}

// checkChainable refuses a previous hook that the generated script could not run the way the runner
// would. The runner matches the extension exactly, so ".SH" is refused here as it would fail there.
func checkChainable(previous string) error {
	switch filepath.Ext(previous) {
	case ".sh", ".ps1":
		return nil
	}
	if previous == "" {
		return nil
	}
	return errorutils.CheckErrorf("the runner's job-started hook '%s' cannot be chained: only .sh and .ps1 hooks can run after "+
		"curate-gh-actions - remove it from %s in the runner's .env, or call it from a .sh or .ps1 script, then install again",
		previous, HookEnvVar)
}

// bashHookScript runs the check first, because the runner's logs it reads are deleted seconds after
// the hook starts, then any hook that was configured before. The runner runs it with 'bash -e', so a
// failed check stops here and fails the job.
func bashHookScript(opts InstallOptions, previous string) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n" + scriptHeader)
	sb.WriteString("export JFROG_CLI_HOME_DIR=" + shellQuote(opts.JfrogHomeDir) + "\n")
	if opts.ServerID != "" {
		sb.WriteString("export JFROG_CLI_SERVER_ID=" + shellQuote(opts.ServerID) + "\n")
	}
	sb.WriteString(shellQuote(opts.JfPath) + " curate-gh-actions --runner-hook --runner-dir " + shellQuote(opts.RunnerDir) + "\n")
	switch filepath.Ext(previous) {
	case ".sh":
		// The runner runs a .sh hook with bash rather than executing it, so the chained hook need not
		// be executable; run it the same way.
		sb.WriteString("bash -e " + shellQuote(previous) + "\n")
	case ".ps1":
		// The runner dot-sources a .ps1 hook with pwsh; off Windows there is no powershell to fall back to.
		sb.WriteString("pwsh -command " + shellQuote(". "+powerShellQuote(previous)) + "\n")
	}
	return sb.String()
}

// powerShellHookScript is the Windows hook. The runner runs it as pwsh -command ". '<path>'" (or
// powershell) and, unlike a run step, adds no exit-code handling around it, so the script stops on
// any error - a missing jf must fail the job, not pass it - and exits non-zero itself when jf does.
// PowerShell then reports any failure as exit code 1, which is all the runner checks. A chained hook
// gets back the default error handling it was written for. The byte order mark is
// there because Windows PowerShell 5.1 reads a file without one in the ANSI code page, which would
// garble a non-ASCII path.
func powerShellHookScript(opts InstallOptions, previous string) string {
	var sb strings.Builder
	sb.WriteString(utf8BOM + scriptHeader)
	sb.WriteString("$ErrorActionPreference = 'Stop'\n")
	sb.WriteString("$env:JFROG_CLI_HOME_DIR = " + powerShellQuote(opts.JfrogHomeDir) + "\n")
	if opts.ServerID != "" {
		sb.WriteString("$env:JFROG_CLI_SERVER_ID = " + powerShellQuote(opts.ServerID) + "\n")
	}
	sb.WriteString("& " + powerShellQuote(opts.JfPath) + " curate-gh-actions --runner-hook --runner-dir " + powerShellQuote(opts.RunnerDir) + "\n")
	sb.WriteString("if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }\n")
	switch filepath.Ext(previous) {
	case ".ps1":
		sb.WriteString("$ErrorActionPreference = 'Continue'\n& " + powerShellQuote(previous) + "\n" + chainedHookFailed)
	case ".sh":
		sb.WriteString("$ErrorActionPreference = 'Continue'\n& bash -e " + powerShellQuote(previous) + "\n" + chainedHookFailed)
	}
	sb.WriteString("exit $LASTEXITCODE\n")
	return sb.String()
}

// chainedHookFailed fails the hook when the chained one did. $LASTEXITCODE reflects only native
// commands, so a chained .ps1 that is missing or ends in an error would otherwise leave jf's 0 there
// and pass the job, where the runner running that hook on its own would fail it.
const chainedHookFailed = "if (-not $?) { if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1 }\n"

// powerShellQuotes are the characters PowerShell takes as a single quote: ' and the typographic ‘ ’ ‚ ‛.
const powerShellQuotes = "'\u2018\u2019\u201a\u201b"

// Inside a single-quoted string each of powerShellQuotes is escaped by doubling it.
var powerShellQuoteEscaper = strings.NewReplacer("'", "''", "\u2018", "\u2018\u2018", "\u2019", "\u2019\u2019", "\u201a", "\u201a\u201a", "\u201b", "\u201b\u201b")

// powerShellQuote returns s as a PowerShell single-quoted string, in which nothing is expanded.
func powerShellQuote(s string) string {
	return "'" + powerShellQuoteEscaper.Replace(s) + "'"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
