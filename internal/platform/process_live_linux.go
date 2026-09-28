//go:build linux

package platform

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// ProcessGroupHasLiveMember reports whether the command's process group still
// has a member that can run. Unlike ProcessGroupExists it does not count a
// member whose every thread is a zombie: such a member runs no code and has
// closed every descriptor, the PTY slave included. It only keeps the group's
// number reserved until whoever adopted it reaps it, which an init that reaps
// lazily, or Relayer itself running as PID 1, may take seconds to do or never
// do at all.
//
// It is only meaningful after the group was sent SIGKILL: the kernel then lets
// no member fork a new one, so a scan that sees nothing but zombies is final.
// It errs towards live whenever /proc cannot show every member of the group.
func ProcessGroupHasLiveMember(command *exec.Cmd) bool {
	if !ProcessGroupExists(command) {
		return false
	}
	if !procShowsEveryProcess() {
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	pgid := command.Process.Pid
	members := 0
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err != nil || pid <= 0 {
			continue
		}
		dir := "/proc/" + entry.Name()
		state, group, err := readStat(dir + "/stat")
		if err != nil {
			if reapedMeanwhile(err) {
				continue
			}
			return true
		}
		if group != pgid {
			continue
		}
		members++
		if !isZombie(state) || threadAlive(dir) {
			return true
		}
	}
	// kill found the group, so a scan that found none of it missed something.
	return members == 0
}

// threadAlive reports whether any thread of the process is not a zombie. A
// leader thread that called pthread_exit shows Z in the process's own stat
// while its other threads still run, so the threads are what count.
func threadAlive(dir string) bool {
	tasks, err := os.ReadDir(dir + "/task")
	if err != nil {
		return !reapedMeanwhile(err)
	}
	for _, task := range tasks {
		state, _, err := readStat(dir + "/task/" + task.Name() + "/stat")
		if err != nil {
			if reapedMeanwhile(err) {
				continue
			}
			return true
		}
		if !isZombie(state) {
			return true
		}
	}
	return false
}

func isZombie(state byte) bool {
	return state == 'Z' || state == 'X'
}

// reapedMeanwhile reports whether a /proc read failed because the process or
// thread was reaped after the directory was listed.
func reapedMeanwhile(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// procShowsEveryProcess reports whether /proc describes this process's own
// PID namespace and hides no process from it. A /proc mounted from another
// namespace, or with hidepid, could leave a live member of the group out of
// the scan.
func procShowsEveryProcess() bool {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return false
	}
	pid, _, found := bytes.Cut(data, []byte(" "))
	if !found || string(pid) != strconv.Itoa(syscall.Getpid()) {
		return false
	}
	return !procHidesProcesses()
}

// procHidesProcesses reports whether the filesystem mounted on /proc was given
// a hidepid option, or cannot be shown not to have been.
func procHidesProcesses() bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return true
	}
	return mountinfoHidesProcesses(string(data))
}

// mountinfoHidesProcesses reads a mountinfo table. The last mount on /proc is
// the one in use; a table with none, or one that is not proc, is not trusted.
func mountinfoHidesProcesses(mountinfo string) bool {
	hides := true
	for _, line := range strings.Split(mountinfo, "\n") {
		mount, super, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		if fields := strings.Fields(mount); len(fields) < 5 || fields[4] != "/proc" {
			continue
		}
		fields := strings.Fields(super)
		hides = len(fields) < 3 || fields[0] != "proc"
		if hides {
			continue
		}
		for _, option := range strings.Split(fields[2], ",") {
			if value, ok := strings.CutPrefix(option, "hidepid="); ok && value != "0" && value != "off" {
				hides = true
			}
		}
	}
	return hides
}

// readStat returns the state and process-group fields of a stat file.
func readStat(path string) (state byte, pgrp int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	state, pgrp, ok := parseStat(data)
	if !ok {
		return 0, 0, errors.New("malformed " + path)
	}
	return state, pgrp, nil
}

// parseStat reads the state and process-group fields of a stat line. The
// command name is parenthesised and may itself contain spaces or parentheses,
// so the fields are counted from the last closing parenthesis.
func parseStat(data []byte) (state byte, pgrp int, ok bool) {
	name := bytes.LastIndexByte(data, ')')
	if name < 0 {
		return 0, 0, false
	}
	// state ppid pgrp ...
	fields := bytes.Fields(data[name+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], pgrp, true
}
