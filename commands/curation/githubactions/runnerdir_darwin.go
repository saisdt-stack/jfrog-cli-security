//go:build darwin

package githubactions

import (
	"bytes"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// newInspector reads processes from the kernel through sysctl, so no helper program is started.
func newInspector() (inspector, error) {
	return inspectSysctl, nil
}

// inspectSysctl reads pid's parent from kern.proc.pid and its executable from kern.procargs2. The
// latter holds the path given to execve, not argv[0], which the parent chooses freely (exec -a).
func inspectSysctl(pid int) (processInfo, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processInfo{}, fmt.Errorf("reading process %d: %w", pid, err)
	}
	if int(kp.Proc.P_pid) != pid {
		// The kernel returns a zeroed record for a pid that does not exist.
		return processInfo{}, fmt.Errorf("process %d does not exist", pid)
	}
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return processInfo{}, fmt.Errorf("reading the arguments of process %d: %w", pid, err)
	}
	// Layout: argc as an int32, then the NUL-terminated exec path.
	if len(args) < 4 {
		return processInfo{}, fmt.Errorf("unexpected kern.procargs2 for process %d", pid)
	}
	exe, _, ok := bytes.Cut(args[4:], []byte{0})
	if !ok {
		return processInfo{}, fmt.Errorf("unexpected kern.procargs2 for process %d", pid)
	}
	info := processInfo{PID: pid, ParentPID: int(kp.Eproc.Ppid)}
	// A relative exec path would have to be resolved against that process's working directory at exec
	// time, which is unknown, so such a process is never taken for the Worker; the walk goes on.
	if p := string(exe); filepath.IsAbs(p) {
		info.Exe = p
	}
	return info, nil
}
