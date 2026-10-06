package githubactions

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	shaV4   = "11d5960a326750d5838078e36cf38b85af677262"
	shaV3   = "a37ce9120846195fa4ece8f58b268e6043cb2f26"
	shaNode = "49933ea5288caeca8642d1e84afbd3f7d6820020"
)

func TestParseSetupJobLines(t *testing.T) {
	text := "2026-10-01T06:57:24.6592950Z Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n" +
		"Download action repository 'actions/checkout@v3' (SHA:" + shaV3 + ")\n" +
		"Download action repository 'actions/setup-node@" + shaNode + "' (SHA:" + shaNode + ")\n" +
		"Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n" +
		"Complete job name: probe\n"
	assert.Equal(t, []LoggedAction{
		{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4},
		{Owner: "actions", Repo: "checkout", Ref: "v3", SHA: shaV3},
		{Owner: "actions", Repo: "setup-node", Ref: shaNode, SHA: shaNode},
	}, ParseSetupJobLines(text))
}

func TestParseWorkerLog(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want []WorkerAction
	}{
		{
			name: "verify when no cache is configured then the download line names the action",
			log: "[2026-10-01 06:43:29Z INFO ActionManager] Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + shaV4 + "' into /r/_work/_actions/_temp_1/x.tar.gz.\n" +
				"[2026-10-01 06:43:31Z INFO ActionManager] Request URL: https://codeload.github.com/actions/checkout/tar.gz/" + shaV4 + " X-GitHub-Request-Id: A Http Status: OK\n",
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4}},
		},
		{
			name: "verify when only the download line is logged then it names the action",
			log:  "Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + shaV4 + "' into /r/_work/_actions/_temp_1/x.tar.gz.\n",
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4}},
		},
		{
			name: "verify when the symlink check names an action then it is listed",
			log: "[x INFO ActionManager] Checking if can symlink 'actions/checkout@" + shaV4 + "'\n" +
				"[x INFO ActionManager] Found unpacked action directory '/c/actions_checkout/" + shaV4 + "' in cache directory '/c'\n",
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4}},
		},
		{
			name: "verify when the archive check names an action then it is listed",
			log: "[x INFO ActionManager] Check if action archive 'actions/setup-node@" + shaNode + "' already exists in cache directory '/c'\n" +
				"[x INFO ActionManager] Found action archive '/c/actions_setup-node/" + shaNode + ".tar.gz' in cache directory '/c'\n",
			want: []WorkerAction{{Owner: "actions", Repo: "setup-node", SHA: shaNode}},
		},
		{
			name: "verify when a symlink attempt falls through to a download then the action is listed once",
			log: "Checking if can symlink 'actions/checkout@" + shaV3 + "'\n" +
				"Check if action archive 'actions/checkout@" + shaV3 + "' already exists in cache directory '/c'\n" +
				"Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + shaV3 + "' into /r/t.tar.gz.\n" +
				"Request URL: https://codeload.github.com/actions/checkout/tar.gz/" + shaV3 + " X-GitHub-Request-Id: B Http Status: OK\n",
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV3}},
		},
		{
			name: "verify when Windows paths and zipball URLs are logged then they are understood",
			log: "Found unpacked action directory 'C:\\cache\\actions_checkout\\" + shaV4 + "' in cache directory 'C:\\cache'\n" +
				"Checking if can symlink 'actions/checkout@" + shaV4 + "'\n" +
				"Save archive 'https://api.github.com/repos/actions/setup-node/zipball/" + shaNode + "' into C:\\r\\t.zip.\n" +
				"Request URL: https://api.github.com/repos/actions/setup-node/zipball/" + shaNode + " X-GitHub-Request-Id: C\n",
			want: []WorkerAction{
				{Owner: "actions", Repo: "checkout", SHA: shaV4},
				{Owner: "actions", Repo: "setup-node", SHA: shaNode},
			},
		},
		{
			name: "verify when one action is checked twice then it is listed once, in first-seen order",
			log: "Checking if can symlink 'actions/checkout@" + shaV4 + "'\n" +
				"Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + shaV3 + "' into x\n" +
				"Checking if can symlink 'Actions/Checkout@" + "11D5960A326750D5838078E36CF38B85AF677262" + "'\n",
			want: []WorkerAction{
				{Owner: "actions", Repo: "checkout", SHA: shaV4},
				{Owner: "actions", Repo: "checkout", SHA: shaV3},
			},
		},
		{
			name: "verify when the log names no action then nothing is returned",
			log:  "[x INFO Worker] Version: 2.337.0\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseWorkerLog(tt.log))
		})
	}
}
