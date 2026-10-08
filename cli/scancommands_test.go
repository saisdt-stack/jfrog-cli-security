package cli

import (
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	commonCommands "github.com/jfrog/jfrog-cli-core/v2/common/commands"
	coretests "github.com/jfrog/jfrog-cli-core/v2/common/tests"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	clienttestutils "github.com/jfrog/jfrog-client-go/utils/tests"

	"github.com/jfrog/build-info-go/utils"
	"github.com/jfrog/jfrog-cli-core/v2/plugins/components"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	flags "github.com/jfrog/jfrog-cli-security/cli/docs"
	"github.com/jfrog/jfrog-cli-security/utils/techutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var TestDataDir = filepath.Join("..", "tests", "testdata")

func TestShouldRunCurationAfterFailure(t *testing.T) {
	tests := []struct {
		name                  string
		cmdName               string
		envSkipCuration       string
		envOutputDirPath      string
		originError           error
		isForbiddenOutput     bool
		isEntitledForCuration bool
		expectedRunCuration   bool
		expectedError         error
	}{
		{
			name:                "Unsupported command",
			cmdName:             "unsupported",
			envOutputDirPath:    "path",
			expectedRunCuration: false,
		},
		{
			name:                "Skip curation after failure",
			cmdName:             "install",
			envSkipCuration:     "true",
			envOutputDirPath:    "path",
			expectedRunCuration: false,
		},
		{
			name:                "Output directory path not set",
			cmdName:             "install",
			envOutputDirPath:    "",
			expectedRunCuration: false,
		},
		{
			name:                "Forbidden error",
			cmdName:             "install",
			originError:         &utils.ForbiddenError{},
			envOutputDirPath:    "path",
			expectedRunCuration: false,
		},
		{
			name:                "Forbidden error in message",
			cmdName:             "install",
			originError:         errors.New("403 Forbidden"),
			envOutputDirPath:    "path",
			expectedRunCuration: false,
		},
		{
			name:                  "Not entitled for curation",
			cmdName:               "install",
			originError:           &utils.ForbiddenError{},
			envOutputDirPath:      "path",
			isEntitledForCuration: false,
			expectedRunCuration:   false,
		},
		{
			name:                  "Successful curation audit",
			cmdName:               "install",
			originError:           &utils.ForbiddenError{},
			envOutputDirPath:      "path",
			isEntitledForCuration: true,
			expectedRunCuration:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variables
			if tt.envSkipCuration != "" {
				callBack := clienttestutils.SetEnvWithCallbackAndAssert(t, SkipCurationAfterFailureEnv, tt.envSkipCuration)
				defer callBack()
			}
			if tt.envOutputDirPath != "" {
				callBack2 := clienttestutils.SetEnvWithCallbackAndAssert(t, coreutils.SummaryOutputDirPathEnv, tt.envOutputDirPath)
				defer callBack2()
			}

			pathToProjectDir := filepath.Join(TestDataDir, "projects", "package-managers", "npm", "npm-project")

			rootDir, err := os.Getwd()
			assert.NoError(t, err)
			tempHomeDir := path.Join(rootDir, path.Join(pathToProjectDir, ".jfrog"))
			callback := clienttestutils.SetEnvWithCallbackAndAssert(t, coreutils.HomeDir, tempHomeDir)
			defer callback()

			serverMock, c, _ := coretests.CreateRtRestsMockServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.String(), "system/version") {
					w.WriteHeader(http.StatusOK)
					_, err := w.Write([]byte(`{"xray_version":"3.99.0"}`))
					assert.NoError(t, err)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, err := w.Write([]byte(`{"feature_id":"curation","entitled":` + strconv.FormatBool(tt.isEntitledForCuration) + `}`))
				assert.NoError(t, err)
			})
			defer serverMock.Close()

			configFilePath := createCliConfig(t, c.ArtifactoryUrl, pathToProjectDir)
			defer func() {
				assert.NoError(t, fileutils.RemoveTempDir(configFilePath))
			}()

			callbackPreTest := clienttestutils.ChangeDirWithCallback(t, rootDir, pathToProjectDir)
			defer callbackPreTest()

			_, err, runCuration := ShouldRunCurationAfterFailure(&components.Context{}, techutils.Npm, tt.cmdName, tt.originError)

			// Verify the expected behavior
			assert.Equal(t, tt.expectedRunCuration, runCuration)
			assert.Equal(t, tt.expectedError, err)

		})
	}
}

func createCliConfig(t *testing.T, url string, configPath string) string {
	server := &config.ServerDetails{
		User:           "admin",
		Password:       "password",
		Url:            url,
		ArtifactoryUrl: url,
		XrayUrl:        url,
	}
	configCmd := commonCommands.NewConfigCommand(commonCommands.AddOrEdit, "test").
		SetDetails(server).SetUseBasicAuthOnly(true).SetInteractive(false)
	assert.NoError(t, configCmd.Run())
	return filepath.Join(configPath, "jfrog-cli.conf.v"+strconv.Itoa(coreutils.GetCliConfigVersion()))
}

func TestCurationAuditCommandFlags_UseWrapperAuditFlag(t *testing.T) {
	// Test that the useWrapperAudit flag is included in the CurationAudit command flags
	curationAuditFlags := flags.GetCommandFlags(flags.CurationAudit)
	found := false
	for _, flag := range curationAuditFlags {
		if flag.GetName() == flags.UseWrapper {
			found = true
			break
		}
	}
	assert.True(t, found, "useWrapperAudit flag should be present in CurationAudit command flags. If this test fails, it means the flag was removed from cli/docs/flags.go")
}

func TestGetCurationCommandRejectsScriptWithWorkingDirs(t *testing.T) {
	ctx := &components.Context{}
	ctx.AddStringFlag(flags.Script, "script.py")
	ctx.AddStringFlag(flags.WorkingDirs, "dir1,dir2")

	_, err := getCurationCommand(ctx)
	assert.ErrorContains(t, err, "--script")
	assert.ErrorContains(t, err, "--working-dirs")
}

func TestGetCurationCommandRejectsScriptWithDockerImage(t *testing.T) {
	ctx := &components.Context{}
	ctx.AddStringFlag(flags.Script, "script.py")
	ctx.AddStringFlag(flags.DockerImageName, "myimage:latest")

	_, err := getCurationCommand(ctx)
	assert.ErrorContains(t, err, "--script")
	assert.ErrorContains(t, err, "--docker-image")
}

func TestEffectiveIncludeViolations(t *testing.T) {
	tests := []struct {
		name            string
		violationsFlag  bool
		projectProvided bool
		want            bool
	}{
		{"default path: flag true, no project", true, false, true},
		{"flag false, no project", false, false, false},
		{"flag false, project overrides", false, true, true},
		{"flag true, project", true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, EffectiveIncludeViolations(tt.violationsFlag, tt.projectProvided))
		})
	}
}

func TestCurationActionsMode(t *testing.T) {
	tests := []struct {
		name                       string
		runnerHook, install, unins bool
		fromPre, fromPost          bool
		trustActionCache           bool
		runnerDir                  string
		want                       curationActionsRunMode
		wantErr                    string
	}{
		{name: "verify when no flag is set then it runs as a step", want: curateAsStep},
		{name: "verify when runner-hook is set with a runner dir then it runs as the hook", runnerHook: true, runnerDir: "/r", want: curateAsHook},
		{name: "verify when runner-hook is set without a runner dir then it fails", runnerHook: true, wantErr: "--runner-dir"},
		{name: "verify when install is set with a runner dir then it installs", install: true, runnerDir: "/r", want: installHook},
		{name: "verify when uninstall is set with a runner dir then it uninstalls", unins: true, runnerDir: "/r", want: uninstallHook},
		{name: "verify when install is set without a runner dir then it fails", install: true, wantErr: "--runner-dir"},
		{name: "verify when two modes are set then it fails", install: true, runnerHook: true, runnerDir: "/r", wantErr: "only one of"},
		{name: "verify when trust-action-cache is set with install then it installs", install: true, trustActionCache: true, runnerDir: "/r", want: installHook},
		{name: "verify when trust-action-cache is set with runner-hook then it runs as the hook", runnerHook: true, trustActionCache: true, runnerDir: "/r", want: curateAsHook},
		{name: "verify when trust-action-cache is set without a hook flag then it fails", trustActionCache: true, wantErr: "--trust-action-cache"},
		{name: "verify when trust-action-cache is set with uninstall then it fails", unins: true, trustActionCache: true, runnerDir: "/r", wantErr: "--trust-action-cache"},
		{name: "verify when from-pre is set alone then it curates from the pre", fromPre: true, want: curateFromPre},
		{name: "verify when from-pre is set with a runner dir then it fails", fromPre: true, runnerDir: "/r", wantErr: "--from-pre"},
		{name: "verify when from-pre is set with runner-hook then it fails", fromPre: true, runnerHook: true, runnerDir: "/r", wantErr: "--from-pre"},
		{name: "verify when from-pre is set with install then it fails", fromPre: true, install: true, runnerDir: "/r", wantErr: "--from-pre"},
		{name: "verify when from-pre is set with uninstall then it fails", fromPre: true, unins: true, runnerDir: "/r", wantErr: "--from-pre"},
		{name: "verify when from-pre is set with trust-action-cache then it fails", fromPre: true, trustActionCache: true, wantErr: "--trust-action-cache"},
		{name: "verify when from-post is set alone then it reports from the post", fromPost: true, want: curateFromPost},
		{name: "verify when from-post is set with from-pre then it fails", fromPost: true, fromPre: true, wantErr: "--from-post"},
		{name: "verify when from-post is set with a runner dir then it fails", fromPost: true, runnerDir: "/r", wantErr: "--from-post"},
		{name: "verify when from-post is set with runner-hook then it fails", fromPost: true, runnerHook: true, runnerDir: "/r", wantErr: "--from-post"},
		{name: "verify when from-post is set with install then it fails", fromPost: true, install: true, runnerDir: "/r", wantErr: "--from-post"},
		{name: "verify when from-post is set with uninstall then it fails", fromPost: true, unins: true, runnerDir: "/r", wantErr: "--from-post"},
		{name: "verify when from-post is set with trust-action-cache then it fails", fromPost: true, trustActionCache: true, wantErr: "--from-post"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := curationActionsMode(tt.runnerHook, tt.install, tt.unins, tt.fromPre, tt.fromPost, tt.trustActionCache, tt.runnerDir)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
