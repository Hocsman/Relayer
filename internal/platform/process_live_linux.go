//go:build linux

package platform

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// groupHoldsOnlyZombies reports whether /proc shows the group's members and
// every one of them is a zombie. Any doubt answers false, which callers read
// as a live member.
func groupHoldsOnlyZombies(pgid int) bool {
	if !procShowsEveryProcess() {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	members := 0
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err != nil || pid <= 0 {
			continue
		}
		state, group, threads, err := readStat("/proc/" + entry.Name() + "/stat")
		if err != nil {
			if reapedMeanwhile(err) {
				continue
			}
			return false
		}
		if group != pgid {
			continue
		}
		members++
		// A leader thread that exited shows Z while the process's other threads
		// still run. The kernel counts a thread until it is released, whereas
		// listing the threads can skip a live one while others exit.
		if !isZombie(state) || threads != 1 {
			return false
		}
	}
	// The caller found the group, so a scan that found none of it missed it.
	return members > 0
}

// ProcessGroupLivenessIsExact reports whether ProcessGroupHasLiveMember can
// tell a group left only with zombies from a live one here, rather than count
// every group that exists as live.
func ProcessGroupLivenessIsExact() bool {
	return procShowsEveryProcess()
}

func isZombie(state byte) bool {
	return state == 'Z' || state == 'X'
}

// reapedMeanwhile reports whether a /proc read failed because the process was
// reaped after the directory was listed.
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

// mountinfoHidesProcesses reads a mountinfo table. The mount in use on /proc
// is the one no other mount on /proc covers, found from the parent IDs: the
// table's order says nothing of it, as a mount moved onto /proc keeps its
// place. A table without exactly one such mount, or whose mount is not proc,
// is not trusted.
func mountinfoHidesProcesses(mountinfo string) bool {
	type procMount struct {
		id    string
		hides bool
	}
	var mounts []procMount
	covered := map[string]bool{}
	for _, line := range strings.Split(mountinfo, "\n") {
		mount, super, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		fields := strings.Fields(mount)
		if len(fields) < 5 || fields[4] != "/proc" {
			continue
		}
		covered[fields[1]] = true
		mounts = append(mounts, procMount{id: fields[0], hides: superblockHidesProcesses(super)})
	}
	hides, top := true, 0
	for _, mount := range mounts {
		if !covered[mount.id] {
			hides, top = mount.hides, top+1
		}
	}
	return top != 1 || hides
}

// superblockHidesProcesses reads the part of a mountinfo line after " - ":
// the filesystem type, the source and the superblock options.
func superblockHidesProcesses(super string) bool {
	fields := strings.Fields(super)
	if len(fields) < 3 || fields[0] != "proc" {
		return true
	}
	for _, option := range strings.Split(fields[2], ",") {
		if value, ok := strings.CutPrefix(option, "hidepid="); ok && value != "0" && value != "off" {
			return true
		}
	}
	return false
}

// readStat returns the state, process-group and thread-count fields of a
// process's stat file.
func readStat(path string) (state byte, pgrp, threads int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	state, pgrp, threads, ok := parseStat(data)
	if !ok {
		return 0, 0, 0, errors.New("malformed " + path)
	}
	return state, pgrp, threads, nil
}

// parseStat reads the state, process-group and thread-count fields of a stat
// line. The command name is parenthesised and may itself contain spaces or
// parentheses, so the fields are counted from the last closing parenthesis.
func parseStat(data []byte) (state byte, pgrp, threads int, ok bool) {
	name := bytes.LastIndexByte(data, ')')
	if name < 0 {
		return 0, 0, 0, false
	}
	// state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt
	// utime stime cutime cstime priority nice num_threads ...
	fields := bytes.Fields(data[name+1:])
	if len(fields) < 18 || len(fields[0]) != 1 {
		return 0, 0, 0, false
	}
	pgrp, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, 0, 0, false
	}
	threads, err = strconv.Atoi(string(fields[17]))
	if err != nil {
		return 0, 0, 0, false
	}
	return fields[0][0], pgrp, threads, true
}
