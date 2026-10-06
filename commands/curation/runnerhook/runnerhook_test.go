package runnerhook

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeRunner(t *testing.T, env string) string {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, flavorFor(runtime.GOOS).configScript), []byte("rem\n"), 0o755))
	if env != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644))
	}
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

func TestInstall(t *testing.T) {
	opts := func(dir string) InstallOptions {
		return InstallOptions{RunnerDir: dir, JfPath: "/usr/local/bin/jf", JfrogHomeDir: "/etc/jfrog", ServerID: "prod"}
	}
	t.Run("verify when installed then the env points at the OS's script that runs hook mode", func(t *testing.T) {
		dir := fakeRunner(t, "LANG=en_US.UTF-8\n")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		flavor := flavorFor(runtime.GOOS)
		script := filepath.Join(dir, ".jfrog", flavor.scriptName)
		assert.Equal(t, "LANG=en_US.UTF-8\n"+HookEnvVar+"="+script+"\n", readFile(t, filepath.Join(dir, ".env")))
		assert.Equal(t, flavor.render(absOpts(t, opts(dir)), ""), readFile(t, script))
	})
	t.Run("verify when installed twice then the env holds one hook line", func(t *testing.T) {
		dir := fakeRunner(t, "")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		_, err = Install(opts(dir))
		require.NoError(t, err)
		assert.Equal(t, HookEnvVar+"="+filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)+"\n", readFile(t, filepath.Join(dir, ".env")))
	})
	t.Run("verify when another hook exists then ours runs first and it runs after", func(t *testing.T) {
		dir := fakeRunner(t, HookEnvVar+"=/opt/other-hook.sh\n")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		content := readFile(t, filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName))
		assert.Equal(t, flavorFor(runtime.GOOS).render(absOpts(t, opts(dir)), "/opt/other-hook.sh"), content)
		assert.Less(t, strings.Index(content, "curate-gh-actions --runner-hook"), strings.Index(content, "/opt/other-hook.sh"))
		assert.Equal(t, "/opt/other-hook.sh", readFile(t, filepath.Join(dir, ".jfrog", previousHookFile)))
	})
	t.Run("verify when run inside a GitHub Actions job then it refuses", func(t *testing.T) {
		dir := fakeRunner(t, "")
		t.Setenv("GITHUB_ACTIONS", "true")
		_, err := Install(opts(dir))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "runner-admin operation")
	})
	t.Run("verify when the directory is not a configured runner then it refuses", func(t *testing.T) {
		t.Setenv("GITHUB_ACTIONS", "")
		_, err := Install(opts(t.TempDir()))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a configured GitHub Actions runner")
	})
	t.Run("verify when the script folder is writable by others then it warns", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows file modes do not say who may write; tamperWarnings' Windows advice is tested directly")
		}
		dir := fakeRunner(t, "")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".jfrog"), 0o777))
		require.NoError(t, os.Chmod(filepath.Join(dir, ".jfrog"), 0o777))
		warnings, err := Install(opts(dir))
		require.NoError(t, err)
		assert.NotEmpty(t, warnings)
	})
}

func TestUninstall(t *testing.T) {
	t.Run("verify when a previous hook was chained then it is restored", func(t *testing.T) {
		dir := fakeRunner(t, "A=1\n"+HookEnvVar+"=/opt/other-hook.sh\n")
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.NoError(t, err)
		require.NoError(t, Uninstall(dir))
		assert.Equal(t, "A=1\n"+HookEnvVar+"=/opt/other-hook.sh\n", readFile(t, filepath.Join(dir, ".env")))
		assert.NoDirExists(t, filepath.Join(dir, ".jfrog"))
	})
	t.Run("verify when no hook was chained then the line is removed", func(t *testing.T) {
		dir := fakeRunner(t, "A=1\n")
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.NoError(t, err)
		require.NoError(t, Uninstall(dir))
		assert.Equal(t, "A=1\n", readFile(t, filepath.Join(dir, ".env")))
	})
}

func TestInstallMakesPathsAbsolute(t *testing.T) {
	t.Chdir(fakeRunner(t, ""))
	// The working directory as the process sees it: on macOS t.TempDir is reached through a symlink.
	dir, err := os.Getwd()
	require.NoError(t, err)
	_, err = Install(InstallOptions{RunnerDir: ".", JfPath: "bin/jf", JfrogHomeDir: "home"})
	require.NoError(t, err)

	script := filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)
	assert.Equal(t, HookEnvVar+"="+script+"\n", readFile(t, filepath.Join(dir, ".env")),
		"the runner resolves the hook path from its own working directory, so it must be absolute")
	content := readFile(t, script)
	// Both script flavors single-quote these paths, so the assertions hold for .sh and .ps1.
	assert.Contains(t, content, "--runner-dir '"+dir+"'")
	assert.Contains(t, content, "'"+filepath.Join(dir, "bin", "jf")+"'")
	assert.Contains(t, content, "'"+filepath.Join(dir, "home")+"'")
}

func TestInstallAcceptsCRLFEnv(t *testing.T) {
	dir := fakeRunner(t, "A=1\r\n"+HookEnvVar+"=/opt/other-hook.sh\r\n")
	_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
	require.NoError(t, err)
	assert.Equal(t, "A=1\n"+HookEnvVar+"="+filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)+"\n", readFile(t, filepath.Join(dir, ".env")),
		"a CRLF .env must not keep its hook line beside ours")
	assert.Equal(t, "/opt/other-hook.sh", readFile(t, filepath.Join(dir, ".jfrog", previousHookFile)),
		"the chained hook path must not carry the carriage return")

	require.NoError(t, Uninstall(dir))
	assert.Equal(t, "A=1\n"+HookEnvVar+"=/opt/other-hook.sh\n", readFile(t, filepath.Join(dir, ".env")))
}

func TestHookScripts(t *testing.T) {
	const header = "# Written by 'jf curate-gh-actions --install-runner-hook'. Re-run that command instead of editing this file.\n"
	unix := InstallOptions{RunnerDir: "/opt/actions-runner", JfPath: "/usr/local/bin/jf", JfrogHomeDir: "/etc/jfrog", ServerID: "prod"}
	windows := InstallOptions{RunnerDir: `C:\actions-runner`, JfPath: `C:\Program Files\jfrog\jf.exe`, JfrogHomeDir: `C:\Users\admin\.jfrog`, ServerID: "prod"}
	tests := []struct {
		name     string
		render   func(InstallOptions, string) string
		opts     InstallOptions
		previous string
		want     string
	}{
		{
			name: "verify when bash has no previous hook and no server then only jf runs", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/r", JfPath: "/jf", JfrogHomeDir: "/h"},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				"'/jf' curate-gh-actions --runner-hook --runner-dir '/r'\n",
		},
		{
			name: "verify when bash chains a .sh hook then bash runs it after jf", render: bashHookScript,
			opts: unix, previous: "/opt/other-hook.sh",
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/etc/jfrog'\n" +
				"export JFROG_CLI_SERVER_ID='prod'\n" +
				"'/usr/local/bin/jf' curate-gh-actions --runner-hook --runner-dir '/opt/actions-runner'\n" +
				"bash -e '/opt/other-hook.sh'\n",
		},
		{
			name: "verify when bash chains a .ps1 hook then pwsh dot-sources it as the runner would", render: bashHookScript,
			opts: unix, previous: "/opt/hooks/other.ps1",
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/etc/jfrog'\n" +
				"export JFROG_CLI_SERVER_ID='prod'\n" +
				"'/usr/local/bin/jf' curate-gh-actions --runner-hook --runner-dir '/opt/actions-runner'\n" +
				`pwsh -command '. '\''/opt/hooks/other.ps1'\'''` + "\n",
		},
		{
			name: "verify when bash paths hold an apostrophe then it is escaped", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/home/o'brien/runner", JfPath: "/jf", JfrogHomeDir: "/h"},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				`'/jf' curate-gh-actions --runner-hook --runner-dir '/home/o'\''brien/runner'` + "\n",
		},
		{
			name: "verify when powershell has no previous hook then it stops on errors and passes jf's exit code on", render: powerShellHookScript,
			opts: InstallOptions{RunnerDir: `C:\r`, JfPath: `C:\jf.exe`, JfrogHomeDir: `C:\h`},
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\h'` + "\n" +
				`& 'C:\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\r'` + "\n" +
				"if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell chains a .ps1 hook then it runs after jf with the runner's default error handling", render: powerShellHookScript,
			opts: windows, previous: `C:\hooks\other.ps1`,
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\Users\admin\.jfrog'` + "\n" +
				"$env:JFROG_CLI_SERVER_ID = 'prod'\n" +
				`& 'C:\Program Files\jfrog\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\actions-runner'` + "\n" +
				"if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }\n" +
				"$ErrorActionPreference = 'Continue'\n" +
				`& 'C:\hooks\other.ps1'` + "\n" +
				"if (-not $?) { if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1 }\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell chains a .sh hook then bash runs it after jf", render: powerShellHookScript,
			opts: windows, previous: `C:\hooks\other.sh`,
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\Users\admin\.jfrog'` + "\n" +
				"$env:JFROG_CLI_SERVER_ID = 'prod'\n" +
				`& 'C:\Program Files\jfrog\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\actions-runner'` + "\n" +
				"if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }\n" +
				"$ErrorActionPreference = 'Continue'\n" +
				`& bash -e 'C:\hooks\other.sh'` + "\n" +
				"if (-not $?) { if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1 }\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell paths hold straight and typographic apostrophes then each is doubled", render: powerShellHookScript,
			opts: InstallOptions{RunnerDir: `C:\Users\O'Brien\r`, JfPath: `C:\Users\O’Brien\jf.exe`, JfrogHomeDir: `C:\h`},
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\h'` + "\n" +
				`& 'C:\Users\O’’Brien\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\Users\O''Brien\r'` + "\n" +
				"if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }\n" +
				"exit $LASTEXITCODE\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.render(tt.opts, tt.previous))
		})
	}
}

func TestFlavorFor(t *testing.T) {
	tests := []struct {
		name, goos, wantScript, wantConfig string
	}{
		{name: "verify when on windows then a .ps1 hook is written for a config.cmd runner", goos: "windows", wantScript: "curate-gh-actions-hook.ps1", wantConfig: "config.cmd"},
		{name: "verify when on linux then a .sh hook is written for a config.sh runner", goos: "linux", wantScript: "curate-gh-actions-hook.sh", wantConfig: "config.sh"},
		{name: "verify when on darwin then a .sh hook is written for a config.sh runner", goos: "darwin", wantScript: "curate-gh-actions-hook.sh", wantConfig: "config.sh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flavorFor(tt.goos)
			assert.Equal(t, tt.wantScript, got.scriptName)
			assert.Equal(t, tt.wantConfig, got.configScript)
		})
	}
}

func TestCheckChainable(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		wantErr  bool
	}{
		{name: "verify when there is no previous hook then nothing is chained", previous: ""},
		{name: "verify when the previous hook is .sh then it is chained", previous: "/opt/a.sh"},
		{name: "verify when the previous hook is .ps1 then it is chained", previous: `C:\hooks\a.ps1`},
		{name: "verify when the previous hook is .js then install refuses", previous: "/opt/a.js", wantErr: true},
		{name: "verify when the previous hook is .cmd then install refuses", previous: `C:\hooks\a.cmd`, wantErr: true},
		{name: "verify when the extension differs only in case then install refuses as the runner would", previous: "/opt/a.SH", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkChainable(tt.previous)
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.previous, "the message must name the hook the admin has to deal with")
		})
	}
}

// absOpts is opts as Install normalizes it, so a test can render the script Install should have written.
func absOpts(t *testing.T, opts InstallOptions) InstallOptions {
	t.Helper()
	for _, path := range []*string{&opts.RunnerDir, &opts.JfPath, &opts.JfrogHomeDir} {
		abs, err := filepath.Abs(*path)
		require.NoError(t, err)
		*path = abs
	}
	return opts
}

func TestInstallRefusesAnUnchainableHook(t *testing.T) {
	env := HookEnvVar + "=/opt/hooks/start.js\n"
	dir := fakeRunner(t, env)
	_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/opt/hooks/start.js")
	assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")), "a refused install must leave the runner's settings as they were")
	assert.NoDirExists(t, filepath.Join(dir, ".jfrog"), "a refused install must not leave a half-written hook behind")
}

func TestTamperWarnings(t *testing.T) {
	opts := InstallOptions{JfPath: `C:\jf\jf.exe`, JfrogHomeDir: `C:\Users\admin\.jfrog`}
	warnings := tamperWarnings("windows", opts, `C:\r\.jfrog`, `C:\r\.jfrog\curate-gh-actions-hook.ps1`)
	require.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], "icacls")
	assert.Contains(t, warnings[0], `C:\jf\jf.exe`)
	assert.Contains(t, warnings[1], `C:\Users\admin\.jfrog`)
	assert.Contains(t, warnings[1], `NETWORK SERVICE`)
}

func TestCheckInstallableUsesTheRunnersConfigureScript(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.sh"), []byte("#!/bin/bash\n"), 0o755))
	err := checkInstallable(dir, flavorFor("windows"))
	require.Error(t, err, "a Windows runner directory holds config.cmd, not config.sh")
	assert.Contains(t, err.Error(), "config.cmd")
	assert.NoError(t, checkInstallable(dir, flavorFor("linux")))
}

func TestCheckHookPath(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		script  string
		wantErr bool
	}{
		{name: "verify when a windows hook path has no quote then it is accepted", goos: "windows", script: `C:\actions runner\.jfrog\curate-gh-actions-hook.ps1`},
		{name: "verify when a windows hook path holds an apostrophe then install refuses", goos: "windows", script: `C:\O'Brien\r\.jfrog\curate-gh-actions-hook.ps1`, wantErr: true},
		{name: "verify when a windows hook path holds a typographic quote then install refuses", goos: "windows", script: `C:\O’Brien\r\.jfrog\curate-gh-actions-hook.ps1`, wantErr: true},
		{name: "verify when a bash hook path holds an apostrophe then it is accepted, since only double quotes group the runner's arguments", goos: "linux", script: "/home/o'brien/r/.jfrog/curate-gh-actions-hook.sh"},
		{name: "verify when a bash hook path holds a space then install refuses", goos: "linux", script: "/opt/actions runner/.jfrog/curate-gh-actions-hook.sh", wantErr: true},
		{name: "verify when a macOS hook path holds a tab then install refuses", goos: "darwin", script: "/Users/a/actions\trunner/.jfrog/curate-gh-actions-hook.sh", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkHookPath(tt.goos, tt.script)
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.script)
		})
	}
}

func TestInstallRefusesARunnerDirTheRunnerCannotStartAHookFrom(t *testing.T) {
	// The runner passes a .ps1 hook's path inside single quotes and a .sh hook's path unquoted, so a
	// quote breaks the first and a space splits the second.
	name := "actions runner"
	if runtime.GOOS == "windows" {
		name = "o'brien"
	}
	dir := filepath.Join(fakeRunner(t, ""), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, flavorFor(runtime.GOOS).configScript), []byte("rem\n"), 0o644))
	_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(dir, ".env"), "a refused install must not point the runner at a hook it cannot start")
}

func TestInstallRecognizesItsOwnHookUnderAnotherSpelling(t *testing.T) {
	// The same runner directory reached through another spelling: a symlink, or on Windows another
	// letter case, which the file system treats as the same path.
	respell := func(t *testing.T, dir string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(dir)
		}
		link := filepath.Join(t.TempDir(), "runner-link")
		require.NoError(t, os.Symlink(dir, link))
		return link
	}
	opts := func(dir string) InstallOptions {
		return InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"}
	}
	flavor := flavorFor(runtime.GOOS)

	t.Run("verify when reinstalled under another spelling then the hook does not chain itself", func(t *testing.T) {
		dir := fakeRunner(t, "")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		other := respell(t, dir)
		_, err = Install(opts(other))
		require.NoError(t, err)

		assert.Equal(t, flavor.render(absOpts(t, opts(other)), ""), readFile(t, filepath.Join(dir, ".jfrog", flavor.scriptName)),
			"a hook that runs itself would recurse on every job")
		assert.NoFileExists(t, filepath.Join(dir, ".jfrog", previousHookFile))
		require.NoError(t, Uninstall(dir))
		assert.NotContains(t, readFile(t, filepath.Join(dir, ".env")), HookEnvVar, "uninstall must not leave the runner pointing at a deleted script")
	})
	t.Run("verify when the remembered previous hook is our own script then it is dropped", func(t *testing.T) {
		dir := fakeRunner(t, "")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		script := filepath.Join(dir, ".jfrog", flavor.scriptName)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".jfrog", previousHookFile), []byte(respell(t, dir)+script[len(dir):]), 0o644))
		_, err = Install(opts(dir))
		require.NoError(t, err)
		assert.Equal(t, flavor.render(absOpts(t, opts(dir)), ""), readFile(t, script))
	})
}

func TestInstallReadsEnvEncodings(t *testing.T) {
	t.Run("verify when .env starts with a UTF-8 byte order mark then its hook is still found", func(t *testing.T) {
		dir := fakeRunner(t, "\ufeff"+HookEnvVar+"=/opt/other-hook.sh\nA=1\n")
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.NoError(t, err)
		assert.Equal(t, "A=1\n"+HookEnvVar+"="+filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)+"\n",
			readFile(t, filepath.Join(dir, ".env")), "the admin's hook must be chained, not left beside ours")
		assert.Equal(t, "/opt/other-hook.sh", readFile(t, filepath.Join(dir, ".jfrog", previousHookFile)))
	})
	t.Run("verify when .env is UTF-16 then install refuses rather than corrupting it", func(t *testing.T) {
		utf16 := "\xff\xfeA\x00=\x001\x00\r\x00\n\x00"
		dir := fakeRunner(t, utf16)
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "UTF-8")
		assert.Equal(t, utf16, readFile(t, filepath.Join(dir, ".env")))
		assert.NoDirExists(t, filepath.Join(dir, ".jfrog"))
	})
}

func TestPreviousHookFileReadErrors(t *testing.T) {
	// A previous-hook file that exists but cannot be read - here a directory in its place, which fails
	// the read on every OS - must not be taken for "nothing was chained", or the admin's hook is lost.
	installed := func(t *testing.T) string {
		dir := fakeRunner(t, HookEnvVar+"=/opt/other-hook.sh\n")
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.NoError(t, err)
		previous := filepath.Join(dir, ".jfrog", previousHookFile)
		require.NoError(t, os.Remove(previous))
		require.NoError(t, os.Mkdir(previous, 0o755))
		return dir
	}
	t.Run("verify when reinstalling cannot read the previous hook then install fails and leaves .env as it was", func(t *testing.T) {
		dir := installed(t)
		env := readFile(t, filepath.Join(dir, ".env"))
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), previousHookFile)
		assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")))
	})
	t.Run("verify when uninstall cannot read the previous hook then it fails and leaves .env as it was", func(t *testing.T) {
		dir := installed(t)
		env := readFile(t, filepath.Join(dir, ".env"))
		err := Uninstall(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), previousHookFile)
		assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")))
	})
}
