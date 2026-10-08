package runnerhook

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeRunner(t *testing.T, env string) string {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv(HookEnvVar, "")
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
		assert.Equal(t, flavor.render(absOpts(t, opts(dir))), readFile(t, script))
	})
	t.Run("verify when installed twice then the env holds one hook line and only the first install warns about a service-set hook", func(t *testing.T) {
		dir := fakeRunner(t, "")
		warnings, err := Install(opts(dir))
		require.NoError(t, err)
		assert.True(t, slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "systemd unit") }),
			"a hook set in the service's environment is replaced by .env, and install cannot see it: %q", warnings)
		warnings, err = Install(opts(dir))
		require.NoError(t, err)
		assert.False(t, slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "systemd unit") }), "%q", warnings)
		assert.Equal(t, HookEnvVar+"="+filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)+"\n", readFile(t, filepath.Join(dir, ".env")))
	})
	t.Run("verify when the admin's hook does not call ours then install writes the script, leaves .env and says what to add", func(t *testing.T) {
		dir := fakeRunner(t, "")
		hook := adminHook(t, "")
		env := HookEnvVar + "=" + hook + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644))
		_, err := Install(opts(dir))
		require.Error(t, err)
		script := filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)
		assert.Contains(t, err.Error(), callLine(hook, script), "the admin must be told the exact line to add")
		assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")), "the runner runs one hook, so the admin's must stay")
		assert.FileExists(t, script, "the line the admin is told to add must work as soon as it is added")
	})
	t.Run("verify when the admin's hook already calls ours then install succeeds and leaves .env", func(t *testing.T) {
		dir := fakeRunner(t, "")
		script := filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)
		hook := adminHook(t, callLine(hookName(), script)+"\n")
		env := HookEnvVar + "=" + hook + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644))
		_, err := Install(opts(dir))
		require.NoError(t, err)
		assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")))
		assert.Equal(t, flavorFor(runtime.GOOS).render(absOpts(t, opts(dir))), readFile(t, script))
	})
	t.Run("verify when only the environment sets the admin's hook then install leaves .env and says what to add", func(t *testing.T) {
		dir := fakeRunner(t, "")
		hook := adminHook(t, "")
		t.Setenv(HookEnvVar, hook)
		_, err := Install(opts(dir))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the "+HookEnvVar+" environment variable")
		assert.Contains(t, err.Error(), callLine(hook, filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)))
		assert.NoFileExists(t, filepath.Join(dir, ".env"), "a .env hook would replace the one the runner gets from its environment")
	})
	t.Run("verify when .env names a hook that does not exist then install refuses and writes nothing", func(t *testing.T) {
		env := HookEnvVar + "=/opt/no-such-hook.sh\n"
		dir := fakeRunner(t, env)
		_, err := Install(opts(dir))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/opt/no-such-hook.sh")
		assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")))
		assert.NoDirExists(t, filepath.Join(dir, ".jfrog"))
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

func TestInstallRunnerHookTrustActionCache(t *testing.T) {
	tests := []struct {
		name  string
		trust bool
	}{
		{name: "verify when installed with --trust-action-cache then the hook script passes it to jf", trust: true},
		{name: "verify when installed without --trust-action-cache then the hook script does not pass it", trust: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := fakeRunner(t, "")
			_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/usr/local/bin/jf", JfrogHomeDir: "/etc/jfrog", TrustActionCache: tt.trust})
			require.NoError(t, err)
			script := readFile(t, filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName))
			assert.Equal(t, tt.trust, strings.Contains(script, "--trust-action-cache"),
				"Install(TrustActionCache: %v) wrote:\n%s", tt.trust, script)
		})
	}
}

func TestUninstall(t *testing.T) {
	install := func(t *testing.T, dir string) {
		t.Helper()
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.NoError(t, err)
	}
	t.Run("verify when .env names our hook then the line and the script are removed", func(t *testing.T) {
		dir := fakeRunner(t, "A=1\n")
		install(t, dir)
		require.NoError(t, Uninstall(dir))
		assert.Equal(t, "A=1\n", readFile(t, filepath.Join(dir, ".env")))
		assert.NoDirExists(t, filepath.Join(dir, ".jfrog"))
	})
	t.Run("verify when the admin's hook still calls ours then uninstall refuses and keeps the script", func(t *testing.T) {
		dir := fakeRunner(t, "")
		hook := adminHook(t, callLine(hookName(), filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName))+"\n")
		env := "A=1\n" + HookEnvVar + "=" + hook + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644))
		install(t, dir)
		err := Uninstall(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), hook)
		assert.DirExists(t, filepath.Join(dir, ".jfrog"), "the admin's hook would fail every job without the script")
		assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")))
	})
	t.Run("verify when the admin's hook no longer calls ours then the script is removed and .env kept", func(t *testing.T) {
		dir := fakeRunner(t, "")
		hook := adminHook(t, callLine(hookName(), filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName))+"\n")
		env := HookEnvVar + "=" + hook + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644))
		install(t, dir)
		require.NoError(t, os.WriteFile(hook, []byte("echo admin\n"), 0o644))
		require.NoError(t, Uninstall(dir))
		assert.NoDirExists(t, filepath.Join(dir, ".jfrog"))
		assert.Equal(t, env, readFile(t, filepath.Join(dir, ".env")))
	})
}

// hookName is the name of an admin's hook on this OS, for callLine.
func hookName() string {
	if runtime.GOOS == "windows" {
		return "admin-hook.ps1"
	}
	return "admin-hook.sh"
}

// adminHook writes an admin's own job-started hook with content and returns its path.
func adminHook(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), hookName())
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
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
	dir := fakeRunner(t, "")
	script := filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\r\n"+HookEnvVar+"="+script+"\r\n"), 0o644))
	_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
	require.NoError(t, err, "the hook path must not carry the carriage return, or our own hook is taken for the admin's")
	assert.Equal(t, "A=1\n"+HookEnvVar+"="+script+"\n", readFile(t, filepath.Join(dir, ".env")),
		"a CRLF .env must not keep its hook line beside ours")
}

func TestHookScripts(t *testing.T) {
	const header = "# Written by 'jf curate-gh-actions --install-runner-hook'. Re-run that command instead of editing this file.\n"
	unix := InstallOptions{RunnerDir: "/opt/actions-runner", JfPath: "/usr/local/bin/jf", JfrogHomeDir: "/etc/jfrog", ServerID: "prod"}
	windows := InstallOptions{RunnerDir: `C:\actions-runner`, JfPath: `C:\Program Files\jfrog\jf.exe`, JfrogHomeDir: `C:\Users\admin\.jfrog`, ServerID: "prod"}
	tests := []struct {
		name   string
		render func(InstallOptions) string
		opts   InstallOptions
		want   string
	}{
		{
			name: "verify when bash has no server then only the home is exported", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/r", JfPath: "/jf", JfrogHomeDir: "/h"},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				"'/jf' curate-gh-actions --runner-hook --runner-dir '/r'\n",
		},
		{
			name: "verify when bash has a server then it is exported for jf", render: bashHookScript,
			opts: unix,
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/etc/jfrog'\n" +
				"export JFROG_CLI_SERVER_ID='prod'\n" +
				"'/usr/local/bin/jf' curate-gh-actions --runner-hook --runner-dir '/opt/actions-runner'\n",
		},
		{
			name: "verify when bash paths hold an apostrophe then it is escaped", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/home/o'brien/runner", JfPath: "/jf", JfrogHomeDir: "/h"},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				`'/jf' curate-gh-actions --runner-hook --runner-dir '/home/o'\''brien/runner'` + "\n",
		},
		{
			name: "verify when bash pins --threads then the hook passes it to jf", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/r", JfPath: "/jf", JfrogHomeDir: "/h", Threads: 8},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				"'/jf' curate-gh-actions --runner-hook --runner-dir '/r' --threads 8\n",
		},
		{
			name: "verify when bash pins --trust-action-cache then the hook passes it to jf", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/r", JfPath: "/jf", JfrogHomeDir: "/h", Threads: 8, TrustActionCache: true},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				"'/jf' curate-gh-actions --runner-hook --runner-dir '/r' --threads 8 --trust-action-cache\n",
		},
		{
			name: "verify when powershell pins --trust-action-cache then the hook passes it to jf", render: powerShellHookScript,
			opts: InstallOptions{RunnerDir: `C:\r`, JfPath: `C:\jf.exe`, JfrogHomeDir: `C:\h`, TrustActionCache: true},
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\h'` + "\n" +
				`& 'C:\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\r' --trust-action-cache` + "\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell pins --threads then the hook passes it to jf", render: powerShellHookScript,
			opts: InstallOptions{RunnerDir: `C:\r`, JfPath: `C:\jf.exe`, JfrogHomeDir: `C:\h`, Threads: 8},
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\h'` + "\n" +
				`& 'C:\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\r' --threads 8` + "\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell has no server then it stops on errors and passes jf's exit code on", render: powerShellHookScript,
			opts: InstallOptions{RunnerDir: `C:\r`, JfPath: `C:\jf.exe`, JfrogHomeDir: `C:\h`},
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\h'` + "\n" +
				`& 'C:\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\r'` + "\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell has a server then it is set for jf", render: powerShellHookScript,
			opts: windows,
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\Users\admin\.jfrog'` + "\n" +
				"$env:JFROG_CLI_SERVER_ID = 'prod'\n" +
				`& 'C:\Program Files\jfrog\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\actions-runner'` + "\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell paths hold straight and typographic apostrophes then each is doubled", render: powerShellHookScript,
			opts: InstallOptions{RunnerDir: `C:\Users\O'Brien\r`, JfPath: `C:\Users\O’Brien\jf.exe`, JfrogHomeDir: `C:\h`},
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\h'` + "\n" +
				`& 'C:\Users\O’’Brien\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\Users\O''Brien\r'` + "\n" +
				"exit $LASTEXITCODE\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.render(tt.opts))
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

func TestCallLine(t *testing.T) {
	tests := []struct {
		name, hook, script, want string
	}{
		{name: "verify when a .sh hook calls the .sh script then bash -e stops it on a failed check", hook: "/opt/a.sh",
			script: "/r/.jfrog/curate-gh-actions-hook.sh", want: "bash -e '/r/.jfrog/curate-gh-actions-hook.sh'"},
		{name: "verify when a .ps1 hook calls the .ps1 script then it checks the result itself", hook: `C:\hooks\a.ps1`,
			script: `C:\r\.jfrog\curate-gh-actions-hook.ps1`, want: `& 'C:\r\.jfrog\curate-gh-actions-hook.ps1'; ` + hookCallFailed},
		{name: "verify when a .ps1 hook calls the .sh script then it runs it with bash and checks the result", hook: "/opt/a.ps1",
			script: "/r/.jfrog/curate-gh-actions-hook.sh", want: `& bash -e '/r/.jfrog/curate-gh-actions-hook.sh'; ` + hookCallFailed},
		{name: "verify when a .sh hook calls the .ps1 script then it runs it with Windows PowerShell, which every Windows has", hook: `C:\hooks\a.sh`,
			script: `C:\r\.jfrog\curate-gh-actions-hook.ps1`, want: `powershell -command '. '\''C:\r\.jfrog\curate-gh-actions-hook.ps1'\'''`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, callLine(tt.hook, tt.script))
		})
	}
}

func TestHookCalls(t *testing.T) {
	tests := []struct {
		name, goos, script, content string
		want                        bool
	}{
		{name: "verify when the hook holds the line install printed then it calls the script", goos: "linux",
			script: "/r/.jfrog/curate-gh-actions-hook.sh", content: "#!/bin/bash\nbash -e '/r/.jfrog/curate-gh-actions-hook.sh'\n", want: true},
		{name: "verify when the hook calls the script without the printed quoting then it still calls it", goos: "linux",
			script: "/r/.jfrog/curate-gh-actions-hook.sh", content: "/r/.jfrog/curate-gh-actions-hook.sh\n", want: true},
		{name: "verify when the path differs in case on linux then it is another file", goos: "linux",
			script: "/r/.jfrog/curate-gh-actions-hook.sh", content: "bash -e /R/.jfrog/curate-gh-actions-hook.sh\n"},
		{name: "verify when the path differs in case on macOS then it is the same file", goos: "darwin",
			script: "/Users/a/r/.jfrog/curate-gh-actions-hook.sh", content: "bash -e /users/A/R/.jfrog/curate-gh-actions-hook.sh\n", want: true},
		{name: "verify when the path differs in case and slashes on windows then it is the same file", goos: "windows",
			script: `C:\Actions-Runner\.jfrog\curate-gh-actions-hook.ps1`, content: "& 'c:/actions-runner/.jfrog/curate-gh-actions-hook.ps1'\n", want: true},
		{name: "verify when the hook does not mention the script then it does not call it", goos: "windows",
			script: `C:\r\.jfrog\curate-gh-actions-hook.ps1`, content: "Write-Host prepare\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := filepath.Join(t.TempDir(), "admin-hook")
			require.NoError(t, os.WriteFile(hook, []byte(tt.content), 0o644))
			got, err := hookCalls(tt.goos, hook, tt.script)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
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

func TestTamperWarnings(t *testing.T) {
	opts := InstallOptions{JfPath: `C:\jf\jf.exe`, JfrogHomeDir: `C:\Users\admin\.jfrog`}
	warnings := tamperWarnings("windows", opts, `C:\r\.jfrog`, `C:\r\.jfrog\curate-gh-actions-hook.ps1`)
	require.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], "icacls")
	assert.Contains(t, warnings[0], `C:\jf\jf.exe`)
	assert.Contains(t, warnings[1], `C:\Users\admin\.jfrog`)
	assert.Contains(t, warnings[1], `NETWORK SERVICE`)
	assert.Contains(t, warnings[1], "JFROG_CLI_HOME_DIR", "the warning must steer to a dedicated configuration, not the admin's own")
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

	t.Run("verify when reinstalled under another spelling then our hook is not taken for the admin's", func(t *testing.T) {
		dir := fakeRunner(t, "")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		other := respell(t, dir)
		_, err = Install(opts(other))
		require.NoError(t, err)

		assert.Equal(t, flavor.render(absOpts(t, opts(other))), readFile(t, filepath.Join(dir, ".jfrog", flavor.scriptName)))
		require.NoError(t, Uninstall(dir))
		assert.NotContains(t, readFile(t, filepath.Join(dir, ".env")), HookEnvVar, "uninstall must not leave the runner pointing at a deleted script")
	})
}

func TestInstallReadsEnvEncodings(t *testing.T) {
	t.Run("verify when .env starts with a UTF-8 byte order mark then its hook is still found", func(t *testing.T) {
		dir := fakeRunner(t, "")
		script := filepath.Join(dir, ".jfrog", flavorFor(runtime.GOOS).scriptName)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("\ufeff"+HookEnvVar+"="+script+"\nA=1\n"), 0o644))
		_, err := Install(InstallOptions{RunnerDir: dir, JfPath: "/jf", JfrogHomeDir: "/h"})
		require.NoError(t, err)
		assert.Equal(t, "A=1\n"+HookEnvVar+"="+script+"\n", readFile(t, filepath.Join(dir, ".env")),
			"our hook must be recognized, not left beside a second line")
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
