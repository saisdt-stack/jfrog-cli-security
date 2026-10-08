//go:build windows

package githubactions

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// newInspector takes one snapshot of the process table for the parent links and reads each exe path
// through the API, so no PowerShell or other program, found through PATH or SystemRoot, is started.
func newInspector() (inspector, error) {
	parents, err := processParents()
	if err != nil {
		return nil, err
	}
	return func(pid int) (processInfo, error) {
		ppid, ok := parents[pid]
		if !ok {
			return processInfo{}, fmt.Errorf("process %d is not in the process snapshot", pid)
		}
		exe, created, err := imageAndCreation(pid)
		if err != nil {
			return processInfo{}, err
		}
		// Windows keeps a dead parent's pid in the child's record and reuses pids, so the pid may now
		// name an unrelated, younger process. A parent that cannot be shown to be older ends the walk.
		if _, parentCreated, err := imageAndCreation(ppid); err != nil || parentCreated > created {
			ppid = 0
		}
		return processInfo{PID: pid, ParentPID: ppid, Exe: exe}, nil
	}, nil
}

// processParents maps every running process to its parent's pid.
func processParents() (parents map[int]int, err error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	defer func() {
		err = errors.Join(err, windows.CloseHandle(snapshot))
	}()
	parents = map[int]int{}
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return parents, nil
}

// imageAndCreation returns the full path of the executable process pid runs and when it was created,
// in nanoseconds since the Unix epoch.
func imageAndCreation(pid int) (path string, created int64, err error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", 0, fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer func() {
		err = errors.Join(err, windows.CloseHandle(process))
	}()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err = windows.QueryFullProcessImageName(process, 0, &buf[0], &size); err != nil {
		return "", 0, fmt.Errorf("reading the executable of process %d: %w", pid, err)
	}
	var creation, exit, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return "", 0, fmt.Errorf("reading the creation time of process %d: %w", pid, err)
	}
	return windows.UTF16ToString(buf[:size]), creation.Nanoseconds(), nil
}
