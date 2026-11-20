//go:build linux

package jshttp

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestKillProcessesKillsDescendants(t *testing.T) {
	// The shell stands in for the Playwright driver and the sleep for a browser
	// it launched.
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	waited := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(waited)
	}()

	buf := make([]byte, 32)

	n, err := out.Read(buf)
	if err != nil {
		t.Fatal(err)
	}

	childPID, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatal(err)
	}

	tree := processTree(cmd.Process.Pid)

	found := false

	for _, p := range tree {
		if p.pid == childPID {
			found = true
		}
	}

	if !found {
		t.Fatalf("processTree(%d) = %v, missing child %d", cmd.Process.Pid, tree, childPID)
	}

	if killed := killProcesses(tree); killed < 2 {
		t.Fatalf("killProcesses killed %d process(es), want at least 2", killed)
	}

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("parent process still running after kill")
	}

	// The orphaned child is re-parented and reaped by init; it must be gone or
	// a zombie, never running.
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if _, _, ok := procStat(childPID); !ok || isZombie(childPID) {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("child %d still running after kill", childPID)
}

func TestKillProcessesSkipsReusedPID(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	tree := processTree(cmd.Process.Pid)
	if len(tree) != 1 {
		t.Fatalf("processTree = %v, want just the process", tree)
	}

	// A different start time means the PID now belongs to another process.
	tree[0].start++

	if killed := killProcesses(tree); killed != 0 {
		t.Fatalf("killProcesses killed %d process(es) with a stale start time", killed)
	}
}

func isZombie(pid int) bool {
	data, err := readStat(pid)
	if err != nil {
		return false
	}

	i := strings.LastIndexByte(data, ')')

	return i >= 0 && strings.HasPrefix(strings.TrimSpace(data[i+1:]), "Z")
}

func readStat(pid int) (string, error) {
	out, err := exec.Command("cat", "/proc/"+strconv.Itoa(pid)+"/stat").Output()

	return string(out), err
}
