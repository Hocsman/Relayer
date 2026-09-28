//go:build linux

package platform

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseStatCountsFieldsFromTheLastParenthesis(t *testing.T) {
	for _, test := range []struct {
		line  string
		state byte
		pgrp  int
	}{
		{"42 (sleep) S 1 42 42 0 -1", 'S', 42},
		{"42 (a) b) Z 1 7 7 0 -1", 'Z', 7},
		{"42 (with space) R 9 13 13 0 -1\n", 'R', 13},
	} {
		state, pgrp, ok := parseStat([]byte(test.line))
		if !ok || state != test.state || pgrp != test.pgrp {
			t.Errorf("parseStat(%q) = %q, %d, %v; want %q, %d", test.line, state, pgrp, ok, test.state, test.pgrp)
		}
	}
	for _, line := range []string{"", "42 sleep S 1 42", "42 (sleep) S 1", "42 (sleep) SS 1 42", "42 (sleep) S 1 x"} {
		if _, _, ok := parseStat([]byte(line)); ok {
			t.Errorf("parseStat(%q) accepted a malformed line", line)
		}
	}
}

func TestMountinfoHidesProcessesOnlyWithoutHidepid(t *testing.T) {
	const root = "22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n"
	for _, test := range []struct {
		name      string
		mountinfo string
		hides     bool
	}{
		{"plain", root + "23 28 0:22 / /proc rw,relatime - proc proc rw\n", false},
		{"hidepid off", root + "23 28 0:22 / /proc rw - proc proc rw,hidepid=0\n", false},
		{"hidepid numeric", root + "23 28 0:22 / /proc rw - proc proc rw,hidepid=2\n", true},
		{"hidepid named", root + "23 28 0:22 / /proc rw - proc proc rw,hidepid=invisible,gid=10\n", true},
		{"remounted with hidepid", root + "23 28 0:22 / /proc rw - proc proc rw\n24 23 0:40 / /proc rw - proc proc rw,hidepid=1\n", true},
		{"remounted without hidepid", root + "23 28 0:22 / /proc rw - proc proc rw,hidepid=1\n24 23 0:40 / /proc rw - proc proc rw\n", false},
		{"not proc", root + "23 28 0:22 / /proc rw - tmpfs tmpfs rw\n", true},
		{"no proc", root, true},
		{"only a path under proc", root + "25 23 0:5 /null /proc/kcore rw - tmpfs tmpfs rw\n", true},
	} {
		if hides := mountinfoHidesProcesses(test.mountinfo); hides != test.hides {
			t.Errorf("%s: mountinfoHidesProcesses = %v, want %v", test.name, hides, test.hides)
		}
	}
}

// TestAZombieGroupHasNoLiveMember leaves a killed child unreaped, as a lazy
// init leaves an orphan: its group still exists but nothing in it can run.
func TestAZombieGroupHasNoLiveMember(t *testing.T) {
	if !procShowsEveryProcess() {
		t.Skip("/proc does not show every process here")
	}
	command := exec.Command("sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = command.Wait() }()

	if !ProcessGroupHasLiveMember(command) {
		t.Fatal("a running group was reported without a live member")
	}
	if err := command.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	stat := "/proc/" + strconv.Itoa(command.Process.Pid) + "/stat"
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(stat)
		if err != nil {
			t.Fatalf("the unreaped child vanished: %v", err)
		}
		if state, _, ok := parseStat(data); ok && state == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the killed child never became a zombie: %s", strings.TrimSpace(string(data)))
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !ProcessGroupExists(command) {
		t.Fatal("the zombie's group was not found; the test no longer reproduces a lazy reap")
	}
	if ProcessGroupHasLiveMember(command) {
		t.Fatal("a group holding only a zombie was reported live")
	}
}
