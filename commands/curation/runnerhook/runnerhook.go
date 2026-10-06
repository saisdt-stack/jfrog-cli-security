// Package runnerhook installs 'jf curate-gh-actions' as a self-hosted runner's job-started hook.
package runnerhook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

const (
	// HookEnvVar is the runner setting that names the job-started hook. The runner reads it from
	// <runner>/.env at start, so a change takes effect when the runner service restarts.
	HookEnvVar   = "ACTIONS_RUNNER_HOOK_JOB_STARTED"
	hookFolder   = ".jfrog"
	scriptHeader = "# Written by 'jf curate-gh-actions --install-runner-hook'. Re-run that command instead of editing this file.\n"
	utf8BOM      = "\ufeff"
)

// InstallOptions are pinned into the hook script, because the runner service usually runs as a
// different user, with a different home, than the admin who ran 'jf config add'.
type InstallOptions struct {
	RunnerDir    string
	JfPath       string // absolute path of the jf binary the hook runs
	JfrogHomeDir string // the admin's JFROG_CLI_HOME_DIR, holding the server configuration
	ServerID     string // "" to use that configuration's default server
	Threads      int    // how many actions the hook decides at once; 0 leaves it to the command's default
}

// Install writes the hook script and points the runner's .env at it. It returns warnings for
// settings that would let a job running on this runner tamper with the check, or keep it from running.
//
// The runner runs a single job-started hook. When another one is already set, Install writes the
// script but leaves the setting alone, and fails until that hook calls the script - see callLine.
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
	lines, inEnvFile, err := readEnv(envPath)
	if err != nil {
		return nil, err
	}
	current := currentHook(envPath, inEnvFile)
	script := filepath.Join(folder, flavor.scriptName)
	// Checked before anything is written, so a refusal leaves the runner as it was.
	if err = checkHookPath(runtime.GOOS, script); err != nil {
		return nil, err
	}
	adminHook := current.path != "" && !samePath(current.path, script)
	calledByAdminHook := false
	if adminHook {
		if calledByAdminHook, err = hookCalls(runtime.GOOS, current.path, script); err != nil {
			return nil, errorutils.CheckErrorf("the runner's job-started hook cannot be read, so nothing was installed.\n\n"+
				"%s sets %s to:\n  %s\nbut reading it failed: %v\n\n"+
				"To continue, either:\n"+
				"  - fix that path so it names your hook, or\n"+
				"  - %s, if the runner should not run that hook,\n"+
				"then run this command again.",
				current.setBy, HookEnvVar, current.path, err, current.unset)
		}
	}
	// #nosec G301 G703 -- the runner service, often another user, must read and run what is in here; the
	// runner directory comes from the admin running install, not from a job
	if err = os.MkdirAll(folder, 0o755); err != nil {
		return nil, errorutils.CheckError(err)
	}
	// #nosec G306 -- the runner service, often another user, must read and run the hook script
	if err = os.WriteFile(script, []byte(flavor.render(opts)), 0o755); err != nil {
		return nil, errorutils.CheckError(err)
	}
	warnings = tamperWarnings(runtime.GOOS, opts, folder, script)
	if !adminHook {
		if err = writeEnv(envPath, lines, script); err != nil {
			return nil, err
		}
		if current.path == "" {
			warnings = append(warnings, fmt.Sprintf("%s now sets %s. If the runner service also sets it in its own environment "+
				"(a systemd unit, a container image or a pod spec), the runner uses the .env value and that hook stops running. "+
				"In that case remove the line from %s and add this as the first command of that hook instead:\n%s",
				envPath, HookEnvVar, envPath, callLine(flavor.scriptName, script)))
		}
	} else if !calledByAdminHook {
		return nil, errorutils.CheckErrorf("one more step is needed: the runner already has a job-started hook, so the curation check is not active yet.\n\n"+
			"A runner runs only one job-started hook, and %s sets it to:\n  %s\n"+
			"This command did not change that. It wrote the curation check to:\n  %s\n"+
			"and your hook has to call it.\n\n"+
			"To finish the install:\n"+
			"  1. Add this line to %s, as its first command:\n       %s\n"+
			"     It must run before anything else in the hook: the runner deletes the logs the check reads a few seconds after a job starts.\n"+
			"  2. Run this command again. It confirms that your hook calls the check.\n\n"+
			"Or, to run only the curation check, %s and run this command again.",
			current.setBy, current.path, script, current.path, callLine(current.path, script), current.unset)
	}
	return warnings, nil
}

// hookSetting is the runner's job-started hook, and where it is set, for telling the admin how to change it.
type hookSetting struct {
	path  string // "" when none is set
	setBy string
	unset string
}

// currentHook is the hook .env sets or, when it sets none, the one this process's environment sets:
// in an image build that is the environment the runner will start with. A hook set only in the runner
// service's own environment cannot be seen from here.
func currentHook(envPath, inEnvFile string) hookSetting {
	if inEnvFile == "" {
		if fromEnv := os.Getenv(HookEnvVar); fromEnv != "" {
			return hookSetting{path: fromEnv, setBy: "the " + HookEnvVar + " environment variable",
				unset: "unset " + HookEnvVar + " where it is set (for example the image's ENV)"}
		}
	}
	return hookSetting{path: inEnvFile, setBy: envPath, unset: "remove the " + HookEnvVar + " line from " + envPath}
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

// Uninstall removes the hook script, and the .env setting when it names the script. It refuses while
// another hook still calls the script, since that hook would then fail every job.
func Uninstall(runnerDir string) error {
	folder := filepath.Join(runnerDir, hookFolder)
	envPath := filepath.Join(runnerDir, ".env")
	lines, inEnvFile, err := readEnv(envPath)
	if err != nil {
		return err
	}
	current := currentHook(envPath, inEnvFile)
	script := filepath.Join(folder, flavorFor(runtime.GOOS).scriptName)
	if current.path == "" || samePath(current.path, script) {
		if err = writeEnv(envPath, lines, ""); err != nil {
			return err
		}
		return errorutils.CheckError(os.RemoveAll(folder))
	}
	called, err := hookCalls(runtime.GOOS, current.path, script)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errorutils.CheckError(fmt.Errorf("reading the runner's job-started hook '%s': %w", current.path, err))
	}
	if called {
		return errorutils.CheckErrorf("the curation check was not removed, because the runner's job-started hook still calls it.\n\n"+
			"%s sets the hook to:\n  %s\nwhich calls:\n  %s\n"+
			"Removing the check first would make that hook, and so every job on this runner, fail.\n\n"+
			"To uninstall:\n"+
			"  1. Remove the line that calls %s from %s.\n"+
			"  2. Run this command again.",
			current.setBy, current.path, script, script, current.path)
	}
	return errorutils.CheckError(os.RemoveAll(folder))
}

// hookCalls reports whether the admin's hook already runs script, so that re-installing does not ask
// for the call again.
func hookCalls(goos, hook, script string) (bool, error) {
	content, err := os.ReadFile(hook)
	if err != nil {
		return false, err
	}
	text := pathText(goos, string(content))
	return strings.Contains(text, pathText(goos, script)) || strings.Contains(text, pathText(goos, callLine(hook, script))), nil
}

// pathText is s as hookCalls compares it: an admin may type the script's path in another letter case
// on Windows and macOS, whose file systems ignore it by default, and with forward slashes on Windows.
func pathText(goos, s string) string {
	switch goos {
	case "windows":
		return strings.ToLower(strings.ReplaceAll(s, "/", `\`))
	case "darwin":
		return strings.ToLower(s)
	}
	return s
}

// callLine is the line that runs script from the admin's hook and stops that hook when the check
// fails. The runner runs a .sh hook with 'bash -e', which stops on a failed command by itself, but
// dot-sources a .ps1 hook with no exit-code handling, so there the line checks the result itself.
// A .ps1 script is only written on Windows, which always has Windows PowerShell but not always pwsh.
func callLine(hook, script string) string {
	if filepath.Ext(hook) == ".ps1" {
		call := "& " + powerShellQuote(script)
		if filepath.Ext(script) == ".sh" {
			call = "& bash -e " + powerShellQuote(script)
		}
		return call + "; " + hookCallFailed
	}
	if filepath.Ext(script) == ".ps1" {
		return "powershell -command " + shellQuote(". "+powerShellQuote(script))
	}
	return "bash -e " + shellQuote(script)
}

// hookCallFailed stops a PowerShell hook when the script it called failed. $LASTEXITCODE reflects only
// native commands and script exits, so a script that ends in an error leaves $? false with no exit code.
const hookCallFailed = "if (-not $?) { if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1 }"

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
// and our own script must not be taken for the admin's hook.
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
	render       func(opts InstallOptions) string
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

// bashHookScript runs the check. The runner runs it with 'bash -e', so a failed check fails the job.
func bashHookScript(opts InstallOptions) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n" + scriptHeader)
	sb.WriteString("export JFROG_CLI_HOME_DIR=" + shellQuote(opts.JfrogHomeDir) + "\n")
	if opts.ServerID != "" {
		sb.WriteString("export JFROG_CLI_SERVER_ID=" + shellQuote(opts.ServerID) + "\n")
	}
	sb.WriteString(shellQuote(opts.JfPath) + " curate-gh-actions --runner-hook --runner-dir " + shellQuote(opts.RunnerDir) + threadsArg(opts) + "\n")
	return sb.String()
}

// powerShellHookScript is the Windows hook. The runner runs it as pwsh -command ". '<path>'" (or
// powershell) and, unlike a run step, adds no exit-code handling around it, so the script stops on
// any error - a missing jf must fail the job, not pass it - and exits with jf's exit code itself.
// PowerShell then reports any failure as exit code 1, which is all the runner checks. The byte order
// mark is there because Windows PowerShell 5.1 reads a file without one in the ANSI code page, which
// would garble a non-ASCII path.
func powerShellHookScript(opts InstallOptions) string {
	var sb strings.Builder
	sb.WriteString(utf8BOM + scriptHeader)
	sb.WriteString("$ErrorActionPreference = 'Stop'\n")
	sb.WriteString("$env:JFROG_CLI_HOME_DIR = " + powerShellQuote(opts.JfrogHomeDir) + "\n")
	if opts.ServerID != "" {
		sb.WriteString("$env:JFROG_CLI_SERVER_ID = " + powerShellQuote(opts.ServerID) + "\n")
	}
	sb.WriteString("& " + powerShellQuote(opts.JfPath) + " curate-gh-actions --runner-hook --runner-dir " + powerShellQuote(opts.RunnerDir) + threadsArg(opts) + "\n")
	sb.WriteString("exit $LASTEXITCODE\n")
	return sb.String()
}

// powerShellQuotes are the characters PowerShell takes as a single quote: ' and the typographic ‘ ’ ‚ ‛.
const powerShellQuotes = "'\u2018\u2019\u201a\u201b"

// Inside a single-quoted string each of powerShellQuotes is escaped by doubling it.
var powerShellQuoteEscaper = strings.NewReplacer("'", "''", "\u2018", "\u2018\u2018", "\u2019", "\u2019\u2019", "\u201a", "\u201a\u201a", "\u201b", "\u201b\u201b")

// powerShellQuote returns s as a PowerShell single-quoted string, in which nothing is expanded.
func powerShellQuote(s string) string {
	return "'" + powerShellQuoteEscaper.Replace(s) + "'"
}

// threadsArg is the --threads the hook passes to jf, or "" so that jf's own default applies.
func threadsArg(opts InstallOptions) string {
	if opts.Threads <= 0 {
		return ""
	}
	return " --threads " + strconv.Itoa(opts.Threads)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
