package runnerhook

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeRunner(t *testing.T, env string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install is not supported on Windows yet")
	}
	t.Setenv("GITHUB_ACTIONS", "")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.sh"), []byte("#!/bin/bash\n"), 0o755))
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
	t.Run("verify when installed then the env points at a script that runs hook mode", func(t *testing.T) {
		dir := fakeRunner(t, "LANG=en_US.UTF-8\n")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		script := filepath.Join(dir, ".jfrog", hookScriptName)
		assert.Equal(t, "LANG=en_US.UTF-8\n"+HookEnvVar+"="+script+"\n", readFile(t, filepath.Join(dir, ".env")))
		content := readFile(t, script)
		assert.Contains(t, content, "export JFROG_CLI_HOME_DIR='/etc/jfrog'\n")
		assert.Contains(t, content, "export JFROG_CLI_SERVER_ID='prod'\n")
		assert.Contains(t, content, "'/usr/local/bin/jf' curate-gh-actions --runner-hook --runner-dir '"+dir+"'\n")
	})
	t.Run("verify when installed twice then the env holds one hook line", func(t *testing.T) {
		dir := fakeRunner(t, "")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		_, err = Install(opts(dir))
		require.NoError(t, err)
		assert.Equal(t, HookEnvVar+"="+filepath.Join(dir, ".jfrog", hookScriptName)+"\n", readFile(t, filepath.Join(dir, ".env")))
	})
	t.Run("verify when another hook exists then ours runs first and it runs after", func(t *testing.T) {
		dir := fakeRunner(t, HookEnvVar+"=/opt/other-hook.sh\n")
		_, err := Install(opts(dir))
		require.NoError(t, err)
		content := readFile(t, filepath.Join(dir, ".jfrog", hookScriptName))
		assert.Less(t, indexOf(content, "curate-gh-actions"), indexOf(content, "/opt/other-hook.sh"))
		assert.Contains(t, content, "bash -e '/opt/other-hook.sh'\n", "the runner runs a .sh hook with bash, so the chained one is run the same way")
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

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
