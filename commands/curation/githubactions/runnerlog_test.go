package githubactions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	shaV4   = "11d5960a326750d5838078e36cf38b85af677262"
	shaV3   = "a37ce9120846195fa4ece8f58b268e6043cb2f26"
	shaNode = "49933ea5288caeca8642d1e84afbd3f7d6820020"
)

func TestParseSetupJobLines(t *testing.T) {
	tests := []struct {
		name string
		text string // the "Set up job" output as the runner buffers it
		want []LoggedAction
	}{
		{
			name: "verify when the runner names actions then each is returned once in the order named",
			text: "2026-10-01T06:57:24.6592950Z Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n" +
				"Download action repository 'actions/checkout@v3' (SHA:" + shaV3 + ")\n" +
				"Download action repository 'actions/setup-node@" + shaNode + "' (SHA:" + shaNode + ")\n" +
				"Download action repository 'actions/checkout@v4' (SHA:" + shaV4 + ")\n" +
				"Complete job name: probe\n",
			want: []LoggedAction{
				{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4},
				{Owner: "actions", Repo: "checkout", Ref: "v3", SHA: shaV3},
				{Owner: "actions", Repo: "setup-node", Ref: shaNode, SHA: shaNode},
			},
		},
		// The line is matched anywhere on a line, so a name broken across lines must not let one capture
		// carry a newline, and with it a workflow command, into what is printed.
		{
			name: "verify when a newline and a workflow command split the ref then nothing is read",
			text: "Download action repository 'actions/checkout@v4\n::error::forged' (SHA:" + shaV4 + ")\n",
		},
		{
			name: "verify when a CRLF and a workflow command split the repository then nothing is read",
			text: "Download action repository 'actions/check\r\n::error::out@v4' (SHA:" + shaV4 + ")\n",
		},
		{
			name: "verify when a newline splits the owner then nothing is read",
			text: "Download action repository 'act\nions/checkout@v4' (SHA:" + shaV4 + ")\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseSetupJobLines(tt.text), "ParseSetupJobLines(%q)", tt.text)
		})
	}
}

// workerLine returns message as the runner writes it into the Worker log.
func workerLine(message string) string {
	return "[2026-10-07 11:03:03Z INFO ActionManager] " + message + "\n"
}

func TestParseWorkerLog(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want []WorkerAction
	}{
		{
			name: "verify when only the download is logged then the action is a save",
			log: workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/"+shaV4+"' into /r/_work/_actions/_temp_1/x.tar.gz.") +
				workerLine("Request URL: https://codeload.github.com/actions/checkout/tar.gz/"+shaV4+" X-GitHub-Request-Id: A Http Status: OK"),
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceSave}},
		},
		{
			name: "verify when the symlink check finds the unpacked directory then it is a cache action in that cache",
			log: workerLine("Checking if can symlink 'actions/checkout@"+shaV4+"'") +
				workerLine("Found unpacked action directory '/c/Actions_Checkout/"+shaV4+"' in cache directory '/c'"),
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache, CacheDir: "/c"}},
		},
		{
			name: "verify when the unpacked directory found is another action's then it gives this one no cache",
			log: workerLine("Checking if can symlink 'actions/checkout@"+shaV4+"'") +
				workerLine("Found unpacked action directory '/c/actions_setup-node/"+shaV4+"' in cache directory '/c'") +
				workerLine("Found unpacked action directory '/c/actions_checkout/"+shaV3+"' in cache directory '/c'"),
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache}},
		},
		{
			name: "verify when the log names two cache directories then no action keeps one, since one must be forged",
			log: workerLine("Check if action archive 'actions/checkout@"+shaV4+"' already exists in cache directory '/c'") +
				workerLine("Check if action archive 'actions/setup-node@"+shaNode+"' already exists in cache directory '/locked'"),
			want: []WorkerAction{
				{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache},
				{Owner: "actions", Repo: "setup-node", SHA: shaNode, Source: SourceCache},
			},
		},
		{
			name: "verify when a found-archive line names another cache directory then no action keeps one",
			log: workerLine("Check if action archive 'actions/checkout@"+shaV4+"' already exists in cache directory '/c'") +
				workerLine("Found action archive '/locked/actions_checkout/"+shaV4+".tar.gz' in cache directory '/locked'"),
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache}},
		},
		{
			name: "verify when the lines name one cache directory spelled differently then each action keeps it",
			log: workerLine("Check if action archive 'actions/checkout@"+shaV4+"' already exists in cache directory '/c/'") +
				workerLine("Found action archive '/c/actions_checkout/"+shaV4+".tar.gz' in cache directory '/c'") +
				workerLine("Checking if can symlink 'actions/setup-node@"+shaNode+"'") +
				workerLine("Found unpacked action directory '/c/actions_setup-node/"+shaNode+"' in cache directory '/c/./'"),
			want: []WorkerAction{
				{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache, CacheDir: "/c/"},
				{Owner: "actions", Repo: "setup-node", SHA: shaNode, Source: SourceCache, CacheDir: "/c/./"},
			},
		},
		{
			name: "verify when a found line lacks the runner prefix then it gives no cache",
			log: workerLine("Checking if can symlink 'actions/checkout@"+shaV4+"'") +
				"[x INFO ActionManager] Found unpacked action directory '/c/actions_checkout/" + shaV4 + "' in cache directory '/c'\n",
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache}},
		},
		{
			name: "verify when the archive check names a cache directory then it is recorded",
			log:  workerLine("Check if action archive 'actions/setup-node@" + shaNode + "' already exists in cache directory '/c'"),
			want: []WorkerAction{{Owner: "actions", Repo: "setup-node", SHA: shaNode, Source: SourceCache, CacheDir: "/c"}},
		},
		{
			name: "verify when the save comes before its check then the action is still a save",
			log: workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/"+shaV3+"' into /r/t.tar.gz.") +
				workerLine("Check if action archive 'actions/checkout@"+shaV3+"' already exists in cache directory '/c'"),
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV3, Source: SourceSave, CacheDir: "/c"}},
		},
		{
			name: "verify when Windows paths, zipball URLs and CRLF line ends are logged then they are understood",
			log: strings.ReplaceAll(workerLine("Check if action archive 'actions/checkout@"+shaV4+"' already exists in cache dir C:\\actionarchivecache\\")+
				workerLine("Save archive 'https://api.github.com/repos/actions/setup-node/zipball/"+shaNode+"' into C:\\r\\t.zip."), "\n", "\r\n"),
			want: []WorkerAction{
				{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache, CacheDir: "C:\\actionarchivecache\\"},
				{Owner: "actions", Repo: "setup-node", SHA: shaNode, Source: SourceSave},
			},
		},
		{
			name: "verify when one action is checked twice in different case then it is listed once, in first-seen order",
			log: workerLine("Checking if can symlink 'actions/checkout@"+shaV4+"'") +
				workerLine("Save archive 'https://codeload.github.com/actions/checkout/tar.gz/"+shaV3+"' into x") +
				workerLine("Checking if can symlink 'Actions/Checkout@11D5960A326750D5838078E36CF38B85AF677262'"),
			want: []WorkerAction{
				{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache},
				{Owner: "actions", Repo: "checkout", SHA: shaV3, Source: SourceSave},
			},
		},
		{
			name: "verify when a line lacks the runner prefix then it is not read",
			log:  "Save archive 'https://codeload.github.com/actions/checkout/tar.gz/" + shaV4 + "' into x\n[x INFO ActionManager] Checking if can symlink 'actions/checkout@" + shaV4 + "'\n",
			want: nil,
		},
		{
			// The job message quotes author-controlled text, which may imitate a runner line after indentation.
			name: "verify when the job message quotes a runner line then it is not read",
			log: "[2026-10-07 11:03:03Z INFO ActionManager] Check if action archive 'actions/checkout@" + sha40 + "' already exists, in cache dir /opt/actionarchivecache\n" +
				"      \"script\": \"[2026-10-07 00:00:00Z INFO ActionManager] Check if action archive 'jobmsg/forged@" + sha40b + "' already exists\"",
			want: []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: sha40, Source: SourceCache, CacheDir: "/opt/actionarchivecache"}},
		},
		{
			name: "verify when the log names no action then nothing is returned",
			log:  "[2026-10-01 06:43:29Z INFO Worker] Version: 2.337.0\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseWorkerLog(tt.log))
		})
	}
}

func TestWorkerLogBuildsImage(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want bool
	}{
		{"verify when the runner counts steps that build an image then it builds", workerLine("2 steps need to build image from 'Dockerfile'"), true},
		{"verify when the runner names the action it builds then it builds", workerLine("Action my-action (abc) from repository 'o/r' needs to build image 'Dockerfile'"), true},
		{"verify when the runner only pulls an image then it does not build", workerLine("Action x (abc) from repository 'o/r' needs to pull image 'alpine:3'"), false},
		{"verify when a build line is only quoted inside a job message then it does not build", "  \"run\": \"[2026-10-07 11:03:03Z INFO ActionManager] 1 steps need to build image from 'Dockerfile'\"\n", false},
		{"verify when the log has no such line then it does not build", workerLine("Checking if can symlink 'actions/checkout@" + shaV4 + "'"), false},
		{"verify when the build line ends in CRLF then it builds", strings.ReplaceAll(workerLine("1 steps need to build image from 'Dockerfile'"), "\n", "\r\n"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, WorkerLogBuildsImage(tt.log))
		})
	}
}
