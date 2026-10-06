package githubactions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const workerStart = "[2026-10-05 05:06:52Z INFO Worker] Version: 2.337.0\n"

func writeDiag(t *testing.T, files map[string]string) string {
	t.Helper()
	runnerDir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(runnerDir, "_diag", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return runnerDir
}

func TestReadRunnerDiag(t *testing.T) {
	download := func(sha string) string {
		return "Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + sha + "' into x\n"
	}
	t.Run("verify when the setup buffer and one Worker log exist then both are read", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"pages/a_1.log":                  "Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n",
			"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
		})
		snap, err := ReadRunnerDiag(dir)
		require.NoError(t, err)
		assert.Equal(t, []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}}, snap.SetupJob)
		assert.Equal(t, []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4}}, snap.Worker)
	})
	t.Run("verify when the setup buffer is already gone then the Worker log is still read", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{"Worker_20261005-050652-utc.log": workerStart + download(shaV4)})
		snap, err := ReadRunnerDiag(dir)
		require.NoError(t, err)
		assert.Empty(t, snap.SetupJob)
		assert.Len(t, snap.Worker, 1)
	})
	t.Run("verify when part of the setup buffer cannot be read then the rest and the Worker log are still read", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"pages":                          "a file where the runner keeps a folder, so it cannot be listed",
			"blocks/b_1.log":                 "Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n",
			"Worker_20261005-050652-utc.log": workerStart + download(shaV4),
		})
		snap, err := ReadRunnerDiag(dir)
		require.NoError(t, err, "the Worker log does not depend on the setup buffer")
		assert.Equal(t, []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}}, snap.SetupJob)
		assert.Equal(t, []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4}}, snap.Worker)
	})
	t.Run("verify when the Worker log rolled over then it reads across a rollover", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{
			"Worker_20261005-044838-utc.log": workerStart + download(shaV3), // an earlier job: must not be read
			"Worker_20261005-050652-utc.log": workerStart + "padding\n",     // this job's first file
			"Worker_20261005-051117-utc.log": download(shaV4),               // rolled file, no start marker
		})
		snap, err := ReadRunnerDiag(dir)
		require.NoError(t, err)
		assert.Equal(t, []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4}}, snap.Worker)
		assert.Equal(t, []string{"Worker_20261005-050652-utc.log", "Worker_20261005-051117-utc.log"}, snap.WorkerFiles)
	})
	t.Run("verify when no Worker log starts a process then it fails", func(t *testing.T) {
		dir := writeDiag(t, map[string]string{"Worker_20261005-051117-utc.log": download(shaV4)})
		_, err := ReadRunnerDiag(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Worker] Version:")
	})
	t.Run("verify when there is no _diag folder then it fails", func(t *testing.T) {
		_, err := ReadRunnerDiag(t.TempDir())
		require.Error(t, err)
	})
}
