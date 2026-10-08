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

// fakeRunner creates a configured runner directory and clears the environment Install reads.
func fakeRunner(t *testing.T, env string) string {
	t.Helper()
	return fakeRunnerAt(t, "", "", env)
}

// fakeRunnerAt is fakeRunner for a runner directory called name under parent. An empty parent is a new
// temporary directory, and an empty name is parent itself.
func fakeRunnerAt(t *testing.T, parent, name, env string) string {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv(HookEnvVar, "")
	dir := parent
	if dir == "" {
		dir = t.TempDir()
	}
	if name != "" {
		dir = filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
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

// installSpelling says whether, and under which spelling of the runner directory, a test installs first.
type installSpelling int

const (
	noReinstall installSpelling = iota
	sameSpelling
	otherSpelling
)

// respell is the same runner directory reached through another spelling: a symlink, or on Windows another
// letter case, which the file system treats as the same path.
func respell(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return strings.ToUpper(dir)
	}
	link := filepath.Join(t.TempDir(), "runner-link")
	require.NoError(t, os.Symlink(dir, link))
	return link
}

// containsWarning reports whether any warning holds substr.
func containsWarning(warnings []string, substr string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, substr) })
}

// skipUnlessGOOS skips a row that is for another OS: "" runs everywhere, "windows" only there and
// "!windows" everywhere else.
func skipUnlessGOOS(t *testing.T, goos string) {
	t.Helper()
	if goos == "windows" && runtime.GOOS != "windows" || goos == "!windows" && runtime.GOOS == "windows" {
		t.Skipf("the row is for %s, not %s", goos, runtime.GOOS)
	}
}

func testOpts(dir string) InstallOptions {
	return InstallOptions{RunnerDir: dir, JfPath: "/usr/local/bin/jf", JfrogHomeDir: "/etc/jfrog", ServerID: "prod"}
}

// scriptIn is the hook script Install writes for a runner in dir.
func scriptIn(dir string) string {
	return filepath.Join(dir, hookFolder, flavorFor(runtime.GOOS).scriptName)
}

func TestInstall(t *testing.T) {
	tests := []struct {
		name string
		// env is .env before install, with {ours}, {admin} and {call} placeholders; "" writes no .env.
		env string
		// adminHook is the admin's own hook content ({call} placeholder); the file is always written,
		// and only rows that name {admin} in env or set adminHookInProcessEnv point the runner at it.
		adminHook string
		// adminHookInProcessEnv sets HookEnvVar={admin} in this process's environment, as an image build would.
		adminHookInProcessEnv bool
		// reinstall runs a first, successful Install before the one under test.
		reinstall installSpelling
		// writableHookFolder pre-creates the hook folder with mode 0o777.
		writableHookFolder bool
		goos               string // "" every OS; "windows" only there; "!windows" everywhere else

		wantErr []string // substrings, placeholders resolved; nil = Install succeeds
		// wantEnv is .env after install, placeholders resolved; "" = .env must not exist.
		wantEnv               string
		wantServiceEnvWarning bool // a warning names the "systemd unit" that would set a hook install replaces
		wantTamperWarning     bool // a warning says a path "is writable by group or others"
	}{
		{
			name:                  "verify when installed on a configured runner then .env keeps its lines, points at the OS's hook script and warns that a service-set hook would be replaced",
			env:                   "LANG=en_US.UTF-8\n",
			wantEnv:               "LANG=en_US.UTF-8\n" + HookEnvVar + "={ours}\n",
			wantServiceEnvWarning: true,
		},
		{
			name:      "verify when installed twice then .env holds one hook line and only the first install warns about a service-set hook",
			reinstall: sameSpelling,
			wantEnv:   HookEnvVar + "={ours}\n",
		},
		{
			name:      "verify when re-installed under another spelling of the runner directory then our hook is not taken for the admin's",
			reinstall: otherSpelling,
			wantEnv:   HookEnvVar + "={ours}\n",
		},
		{
			name:    "verify when .env has CRLF line endings then our hook line is recognized and written once",
			env:     "A=1\r\n" + HookEnvVar + "={ours}\r\n",
			wantEnv: "A=1\n" + HookEnvVar + "={ours}\n",
		},
		{
			name:    "verify when .env starts with a UTF-8 byte order mark then our hook line is recognized",
			env:     "\ufeff" + HookEnvVar + "={ours}\nA=1\n",
			wantEnv: "A=1\n" + HookEnvVar + "={ours}\n",
		},
		{
			name:      "verify when the admin's hook already calls ours then install succeeds and leaves .env",
			env:       HookEnvVar + "={admin}\n",
			adminHook: "{call}\n",
			wantEnv:   HookEnvVar + "={admin}\n",
		},
		{
			name:      "verify when the admin's hook does not call ours then install writes the script, leaves .env and names the line to add",
			env:       HookEnvVar + "={admin}\n",
			adminHook: "",
			wantErr:   []string{"{call}"},
			wantEnv:   HookEnvVar + "={admin}\n",
		},
		{
			name:                  "verify when only the process environment sets the admin's hook then install writes no .env and names the line to add",
			adminHookInProcessEnv: true,
			wantErr:               []string{"the " + HookEnvVar + " environment variable", "{call}"},
		},
		{
			name:                  "verify when the hook folder is writable by others then install warns",
			goos:                  "!windows",
			writableHookFolder:    true,
			wantEnv:               HookEnvVar + "={ours}\n",
			wantServiceEnvWarning: true,
			wantTamperWarning:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skipUnlessGOOS(t, tt.goos)
			dir := fakeRunner(t, "")
			admin := adminHook(t)
			if tt.adminHookInProcessEnv {
				t.Setenv(HookEnvVar, admin)
			}
			if tt.writableHookFolder {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, hookFolder), 0o777))
				require.NoError(t, os.Chmod(filepath.Join(dir, hookFolder), 0o777))
			}
			if tt.reinstall != noReinstall {
				_, err := Install(testOpts(dir))
				require.NoError(t, err)
			}
			runnerDir := dir
			if tt.reinstall == otherSpelling {
				runnerDir = respell(t, dir)
			}
			ours := scriptIn(runnerDir)
			resolve := strings.NewReplacer("{ours}", ours, "{admin}", admin, "{call}", callLine(admin, ours)).Replace
			require.NoError(t, os.WriteFile(admin, []byte(resolve(tt.adminHook)), 0o644))
			if tt.env != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(resolve(tt.env)), 0o644))
			}

			warnings, err := Install(testOpts(runnerDir))

			if tt.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				for _, want := range tt.wantErr {
					assert.Contains(t, err.Error(), resolve(want))
				}
			}
			assert.Equal(t, flavorFor(runtime.GOOS).render(absOpts(t, testOpts(runnerDir))), readFile(t, filepath.Join(dir, hookFolder, flavorFor(runtime.GOOS).scriptName)),
				"the line the admin is told to add must work as soon as it is added")
			if tt.wantEnv == "" {
				assert.NoFileExists(t, filepath.Join(dir, ".env"))
			} else {
				assert.Equal(t, resolve(tt.wantEnv), readFile(t, filepath.Join(dir, ".env")))
			}
			assert.Equal(t, tt.wantServiceEnvWarning, containsWarning(warnings, "systemd unit"), "%q", warnings)
			assert.Equal(t, tt.wantTamperWarning, containsWarning(warnings, "writable by group or others"), "%q", warnings)
			// On Windows the file mode says nothing about who may write, so every successful install tells the
			// admin to lock the hook down instead; no other warning uses this wording.
			assert.Equal(t, runtime.GOOS == "windows" && tt.wantErr == nil, containsWarning(warnings, "file permissions are not checked"), "%q", warnings)
		})
	}
}

func TestInstallRefusesAndChangesNothing(t *testing.T) {
	// The runner passes a .ps1 hook's path inside single quotes and a .sh hook's path unquoted, so a
	// quote breaks the first and a space splits the second.
	tests := []struct {
		name       string
		env        string // .env before install; "" writes no .env
		inJob      bool   // GITHUB_ACTIONS=true
		notARunner bool   // no .runner and no configure script
		// runnerDirName is the runner directory's base name; "" uses a plain temp directory.
		runnerDirName string
		goos          string // "" every OS; "windows" only there; "!windows" everywhere else
		wantErr       string
	}{
		{
			name:    "verify when run inside a GitHub Actions job then install refuses and changes nothing",
			inJob:   true,
			wantErr: "runner-admin operation",
		},
		{
			name:       "verify when the directory is not a configured runner then install refuses and changes nothing",
			notARunner: true,
			wantErr:    "not a configured GitHub Actions runner",
		},
		{
			name:          "verify when the runner path holds whitespace then install refuses and changes nothing",
			runnerDirName: "actions runner",
			goos:          "!windows",
			wantErr:       "cannot start a hook",
		},
		{
			name:          "verify when the runner path holds an apostrophe on Windows then install refuses and changes nothing",
			runnerDirName: "o'brien",
			goos:          "windows",
			wantErr:       "cannot start a hook",
		},
		{
			name:    "verify when .env is UTF-16 then install refuses rather than corrupting it",
			env:     "\xff\xfeA\x00=\x001\x00\r\x00\n\x00",
			wantErr: "UTF-8",
		},
		{
			name:    "verify when .env names a hook that does not exist then install refuses and changes nothing",
			env:     HookEnvVar + "=/opt/no-such-hook.sh\n",
			wantErr: "/opt/no-such-hook.sh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skipUnlessGOOS(t, tt.goos)
			var dir string
			if tt.notARunner {
				t.Setenv(HookEnvVar, "")
				dir = t.TempDir()
			} else {
				dir = fakeRunnerAt(t, "", tt.runnerDirName, tt.env)
			}
			if tt.inJob {
				t.Setenv("GITHUB_ACTIONS", "true")
			} else {
				t.Setenv("GITHUB_ACTIONS", "")
			}

			_, err := Install(testOpts(dir))

			require.ErrorContains(t, err, tt.wantErr)
			if tt.env == "" {
				assert.NoFileExists(t, filepath.Join(dir, ".env"))
			} else {
				assert.Equal(t, tt.env, readFile(t, filepath.Join(dir, ".env")))
			}
			assert.NoDirExists(t, filepath.Join(dir, hookFolder), "a refused install must not write a hook the runner could be pointed at")
		})
	}
}

func TestUninstall(t *testing.T) {
	tests := []struct {
		name string
		env  string // .env before install, with {admin} placeholder
		// adminHook is the admin's hook content at uninstall time ({call} placeholder). During install
		// the hook always calls ours, so install succeeds.
		adminHook string
		installAs installSpelling // sameSpelling | otherSpelling; uninstall always uses the original
		// adminHookIsDir replaces the admin's hook with a directory, which cannot be read as a file.
		adminHookIsDir bool
		wantErr        string // substring, placeholders resolved; "" = Uninstall succeeds
		// wantEnv is .env after uninstall, placeholders resolved; unlike TestInstall's, "" means .env exists and is empty.
		wantEnv string
		// wantScriptKept is whether the hook folder is still there.
		wantScriptKept bool
	}{
		{
			name:      "verify when .env names our hook then the line and the script are removed",
			env:       "A=1\n",
			installAs: sameSpelling,
			wantEnv:   "A=1\n",
		},
		{
			name:      "verify when .env names our hook under another spelling then the line and the script are removed",
			installAs: otherSpelling,
			wantEnv:   "",
		},
		{
			name:           "verify when the admin's hook still calls ours then uninstall refuses and keeps the script",
			env:            "A=1\n" + HookEnvVar + "={admin}\n",
			adminHook:      "{call}\n",
			installAs:      sameSpelling,
			wantErr:        "{admin}",
			wantEnv:        "A=1\n" + HookEnvVar + "={admin}\n",
			wantScriptKept: true,
		},
		{
			name:      "verify when the admin's hook no longer calls ours then the script is removed and .env kept",
			env:       HookEnvVar + "={admin}\n",
			adminHook: "echo admin\n",
			installAs: sameSpelling,
			wantEnv:   HookEnvVar + "={admin}\n",
		},
		{
			name:           "verify when the admin's hook cannot be read then uninstall errors and keeps the script and .env",
			env:            HookEnvVar + "={admin}\n",
			installAs:      sameSpelling,
			adminHookIsDir: true,
			wantErr:        "reading the runner's job-started hook",
			wantEnv:        HookEnvVar + "={admin}\n",
			wantScriptKept: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := fakeRunner(t, "")
			installDir := dir
			if tt.installAs == otherSpelling {
				installDir = respell(t, dir)
			}
			admin := adminHook(t)
			resolve := strings.NewReplacer("{admin}", admin, "{call}", callLine(admin, scriptIn(installDir))).Replace
			require.NoError(t, os.WriteFile(admin, []byte(resolve("{call}\n")), 0o644))
			if tt.env != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(resolve(tt.env)), 0o644))
			}
			_, err := Install(testOpts(installDir))
			require.NoError(t, err)
			if tt.adminHookIsDir {
				require.NoError(t, os.Remove(admin))
				require.NoError(t, os.Mkdir(admin, 0o755))
			} else {
				require.NoError(t, os.WriteFile(admin, []byte(resolve(tt.adminHook)), 0o644))
			}

			err = Uninstall(dir)

			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, resolve(tt.wantErr))
			}
			assert.Equal(t, resolve(tt.wantEnv), readFile(t, filepath.Join(dir, ".env")))
			if tt.wantScriptKept {
				assert.DirExists(t, filepath.Join(dir, hookFolder), "the admin's hook would fail every job without the script")
			} else {
				assert.NoDirExists(t, filepath.Join(dir, hookFolder))
			}
		})
	}
}

// hookName is the name of an admin's hook on this OS, for callLine.
func hookName() string {
	if runtime.GOOS == "windows" {
		return "admin-hook.ps1"
	}
	return "admin-hook.sh"
}

// adminHook returns the path of an admin's own job-started hook in a fresh directory; the caller
// writes its content once the placeholders that depend on the path can be resolved.
func adminHook(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), hookName())
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

func TestHookScripts(t *testing.T) {
	const header = "# Written by 'jf curate-gh-actions --install-runner-hook'. Re-run that command instead of editing this file.\n"
	unix := InstallOptions{RunnerDir: "/opt/actions-runner", JfPath: "/usr/local/bin/jf", JfrogHomeDir: "/etc/jfrog", ServerID: "prod", Threads: 8, TrustActionCache: true}
	windows := InstallOptions{RunnerDir: `C:\actions-runner`, JfPath: `C:\Program Files\jfrog\jf.exe`, JfrogHomeDir: `C:\Users\admin\.jfrog`, ServerID: "prod", Threads: 8, TrustActionCache: true}
	tests := []struct {
		name   string
		render func(InstallOptions) string
		opts   InstallOptions
		want   string
	}{
		{
			name: "verify when bash has only the required options then it exports the home and passes no optional flag", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/r", JfPath: "/jf", JfrogHomeDir: "/h"},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				"'/jf' curate-gh-actions --runner-hook --runner-dir '/r'\n",
		},
		{
			name: "verify when bash has a server, threads and trust-action-cache then each reaches jf", render: bashHookScript,
			opts: unix,
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/etc/jfrog'\n" +
				"export JFROG_CLI_SERVER_ID='prod'\n" +
				"'/usr/local/bin/jf' curate-gh-actions --runner-hook --runner-dir '/opt/actions-runner' --threads 8 --trust-action-cache\n",
		},
		{
			name: "verify when bash paths hold an apostrophe then it is escaped", render: bashHookScript,
			opts: InstallOptions{RunnerDir: "/home/o'brien/runner", JfPath: "/jf", JfrogHomeDir: "/h"},
			want: "#!/bin/bash\n" + header +
				"export JFROG_CLI_HOME_DIR='/h'\n" +
				`'/jf' curate-gh-actions --runner-hook --runner-dir '/home/o'\''brien/runner'` + "\n",
		},
		{
			name: "verify when powershell has only the required options then it stops on errors and passes jf's exit code on", render: powerShellHookScript,
			opts: InstallOptions{RunnerDir: `C:\r`, JfPath: `C:\jf.exe`, JfrogHomeDir: `C:\h`},
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\h'` + "\n" +
				`& 'C:\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\r'` + "\n" +
				"exit $LASTEXITCODE\n",
		},
		{
			name: "verify when powershell has a server, threads and trust-action-cache then each reaches jf", render: powerShellHookScript,
			opts: windows,
			want: "\ufeff" + header +
				"$ErrorActionPreference = 'Stop'\n" +
				`$env:JFROG_CLI_HOME_DIR = 'C:\Users\admin\.jfrog'` + "\n" +
				"$env:JFROG_CLI_SERVER_ID = 'prod'\n" +
				`& 'C:\Program Files\jfrog\jf.exe' curate-gh-actions --runner-hook --runner-dir 'C:\actions-runner' --threads 8 --trust-action-cache` + "\n" +
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
