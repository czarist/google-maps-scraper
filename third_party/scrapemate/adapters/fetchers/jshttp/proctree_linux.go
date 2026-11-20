//go:build linux

package jshttp

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procID identifies a process by PID and start time, so a PID reused by an
// unrelated process after the original one exited is never killed.
type procID struct {
	pid   int
	start uint64
}

// procStat returns the parent PID and start time of pid from /proc.
func procStat(pid int) (ppid int, start uint64, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}

	// The command name (field 2) may contain spaces; fields after it follow the
	// last ')'. After it: state(0) ppid(1) ... starttime(19).
	s := string(data)

	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}

	fields := strings.Fields(s[i+1:])

	const startTimeField = 19
	if len(fields) <= startTimeField {
		return 0, 0, false
	}

	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}

	start, err = strconv.ParseUint(fields[startTimeField], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	return ppid, start, true
}

// processTree returns root and all of its descendants (the Playwright driver
// and the browsers it launched). It returns nil if root is not running.
func processTree(root int) []procID {
	if root <= 0 {
		return nil
	}

	_, rootStart, ok := procStat(root)
	if !ok {
		return nil
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return []procID{{pid: root, start: rootStart}}
	}

	children := make(map[int][]procID)

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		ppid, start, ok := procStat(pid)
		if !ok {
			continue
		}

		children[ppid] = append(children[ppid], procID{pid: pid, start: start})
	}

	tree := []procID{{pid: root, start: rootStart}}

	for i := 0; i < len(tree); i++ {
		tree = append(tree, children[tree[i].pid]...)
	}

	return tree
}

// killProcesses sends SIGKILL to every process in procs that is still the same
// process (same start time). Descendants are killed before their parents.
func killProcesses(procs []procID) int {
	killed := 0

	for i := len(procs) - 1; i >= 0; i-- {
		p := procs[i]

		_, start, ok := procStat(p.pid)
		if !ok || start != p.start {
			continue
		}

		if syscall.Kill(p.pid, syscall.SIGKILL) == nil {
			killed++
		}
	}

	return killed
}
