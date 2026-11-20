//go:build !linux

package jshttp

import "os"

// procID identifies a process. Outside Linux only the driver itself is
// tracked; the browsers it launched are not.
type procID struct {
	pid int
}

func processTree(root int) []procID {
	if root <= 0 {
		return nil
	}

	return []procID{{pid: root}}
}

func killProcesses(procs []procID) int {
	killed := 0

	for _, p := range procs {
		proc, err := os.FindProcess(p.pid)
		if err != nil {
			continue
		}

		if proc.Kill() == nil {
			killed++
		}
	}

	return killed
}
