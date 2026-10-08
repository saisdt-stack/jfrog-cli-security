//go:build linux

package githubactions

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// newInspector reads processes from /proc, so no helper program is started.
func newInspector() (inspector, error) {
	return inspectProc, nil
}

// inspectProc reads pid's parent from /proc/<pid>/stat and its executable from the /proc/<pid>/exe link,
// which the kernel resolves, so the process cannot name itself.
func inspectProc(pid int) (processInfo, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processInfo{}, err
	}
	// The command name in parentheses may itself hold ") ", so the fields start after the last ')':
	// state, then the parent pid.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return processInfo{}, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 2 {
		return processInfo{}, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return processInfo{}, fmt.Errorf("parent pid in /proc/%d/stat: %w", pid, err)
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return processInfo{}, err
	}
	// The kernel appends this when the binary was replaced on disk after the process started, as on a
	// runner self-update.
	return processInfo{PID: pid, ParentPID: ppid, Exe: strings.TrimSuffix(exe, " (deleted)")}, nil
}
