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
	// The fields after the name, up to num_threads.
	const tail = " 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 "
	for _, test := range []struct {
		line    string
		state   byte
		pgrp    int
		threads int
	}{
		{"42 (sleep) S 1 42 42" + tail + "1 0 7", 'S', 42, 1},
		{"42 (a) b) Z 1 7 7" + tail + "1 0 7", 'Z', 7, 1},
		{"42 (with space) R 9 13 13" + tail + "12 0 7\n", 'R', 13, 12},
		{"42 (node) Z 9 13 13" + tail + "3", 'Z', 13, 3},
	} {
		state, pgrp, threads, ok := parseStat([]byte(test.line))
		if !ok || state != test.state || pgrp != test.pgrp || threads != test.threads {
			t.Errorf("parseStat(%q) = %q, %d, %d, %v; want %q, %d, %d", test.line, state, pgrp, threads, ok, test.state, test.pgrp, test.threads)
		}
	}
	for _, line := range []string{
		"",
		"42 sleep S 1 42 42" + tail + "1",
		"42 (sleep) S 1 42 42",
		"42 (sleep) S 1 42 42" + strings.TrimSpace(tail),
		"42 (sleep) SS 1 42 42" + tail + "1",
		"42 (sleep) S 1 x 42" + tail + "1",
		"42 (sleep) S 1 42 42" + tail + "x",
	} {
		if _, _, _, ok := parseStat([]byte(line)); ok {
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
		{"plain", root + "23 22 0:22 / /proc rw,relatime - proc proc rw\n", false},
		{"hidepid off", root + "23 22 0:22 / /proc rw - proc proc rw,hidepid=0\n", false},
		{"hidepid numeric", root + "23 22 0:22 / /proc rw - proc proc rw,hidepid=2\n", true},
		{"hidepid named", root + "23 22 0:22 / /proc rw - proc proc rw,hidepid=invisible,gid=10\n", true},
		{"remounted with hidepid", root + "23 22 0:22 / /proc rw - proc proc rw\n24 23 0:40 / /proc rw - proc proc rw,hidepid=1\n", true},
		{"remounted without hidepid", root + "23 22 0:22 / /proc rw - proc proc rw,hidepid=1\n24 23 0:40 / /proc rw - proc proc rw\n", false},
		// mount --move keeps the moved mount's place in the table.
		{"moved on top with hidepid", root + "23 22 0:22 / /proc rw - proc proc rw\n21 23 0:40 / /proc rw - proc proc rw,hidepid=invisible\n", true},
		{"moved on top without hidepid", root + "23 22 0:22 / /proc rw - proc proc rw,hidepid=2\n21 23 0:40 / /proc rw - proc proc rw\n", false},
		{"two mounts on top", root + "23 22 0:22 / /proc rw - proc proc rw\n24 22 0:40 / /proc rw - proc proc rw\n", true},
		{"not proc", root + "23 22 0:22 / /proc rw - tmpfs tmpfs rw\n", true},
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
	if !ProcessGroupLivenessIsExact() {
		t.Skip("/proc does not show every process here")
	}
	command := exec.Command("sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()

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
		if state, _, _, ok := parseStat(data); ok && state == 'Z' {
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

// TestAGroupWhoseLeaderIsGoneIsLiveWhileAMemberRuns: once the leader has been
// reaped another member can still run and hold the PTY slave.
func TestAGroupWhoseLeaderIsGoneIsLiveWhileAMemberRuns(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "sleep 30 & exit 0")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := command.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	if err := command.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if !ProcessGroupExists(command) {
		t.Fatal("the group vanished while a member still runs")
	}
	if !ProcessGroupHasLiveMember(command) || !ProcessGroupIDHasLiveMember(pgid) {
		t.Fatal("a group whose leader was reaped but whose member still runs was reported without a live member")
	}
}
