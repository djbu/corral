package procinfo

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestStartTimeSelfNonZeroAndStable is design doc §10.2's procinfo row:
// StartTime(os.Getpid()) non-zero and stable (repeated calls against the
// same live process agree).
func TestStartTimeSelfNonZeroAndStable(t *testing.T) {
	pid := os.Getpid()

	a, err := StartTime(pid)
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	if a == 0 {
		t.Fatal("StartTime(self) = 0, want non-zero")
	}

	b, err := StartTime(pid)
	if err != nil {
		t.Fatalf("StartTime(self) second call: %v", err)
	}
	if a != b {
		t.Fatalf("StartTime(self) not stable: %d != %d", a, b)
	}
}

// TestAliveWrongStartTime checks that a pid which is genuinely running
// still reports not-alive against a start time that does not match its
// real one — the pid-reuse guard Alive exists for.
func TestAliveWrongStartTime(t *testing.T) {
	pid := os.Getpid()

	real, err := StartTime(pid)
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	if !Alive(pid, real) {
		t.Fatal("Alive(self, real start time) = false, want true")
	}
	if Alive(pid, real+1) {
		t.Fatal("Alive(self, wrong start time) = true, want false")
	}
	if Alive(pid, 0) {
		t.Fatal("Alive(self, 0) = true, want false (0 must never match)")
	}
}

// TestAliveReapedChildIsFalse spawns a short-lived child, captures its
// start time while it is genuinely running, waits for it to exit (which
// reaps it), and asserts Alive then reports false — a pid whose process
// table entry is entirely gone must never be mistaken for alive.
func TestAliveReapedChildIsFalse(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 0.2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	pid := cmd.Process.Pid

	startNs, err := StartTime(pid)
	if err != nil {
		t.Fatalf("StartTime(live child): %v", err)
	}
	if !Alive(pid, startNs) {
		t.Fatal("Alive(live child, its own start time) = false, want true")
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for child: %v", err)
	}

	if Alive(pid, startNs) {
		t.Fatal("Alive(reaped child, its former start time) = true, want false")
	}
}

// TestKillGroupRefusesLowPgid covers pgid <= 1: 0/negative address
// something other than an intended child's group under kill(2), and 1 is
// init's process group.
func TestKillGroupRefusesLowPgid(t *testing.T) {
	for _, pgid := range []int{-1, 0, 1} {
		if err := KillGroup(pgid, syscall.SIGTERM); err == nil {
			t.Errorf("KillGroup(%d, SIGTERM) = nil error, want refusal", pgid)
		}
	}
}

// TestKillGroupRefusesOwnPgid covers the other guard: never let a caller
// accidentally signal (and, for SIGKILL, terminate) the process that is
// doing the supervising.
func TestKillGroupRefusesOwnPgid(t *testing.T) {
	own := syscall.Getpgrp()
	if err := KillGroup(own, syscall.SIGTERM); err == nil {
		t.Fatalf("KillGroup(%d, SIGTERM) [our own pgid] = nil error, want refusal", own)
	}
}

// TestKillGroupSignalsARealGroup is a positive control for the two
// refusal tests above: a real, unrelated process group (a child started
// via Setpgid so it is its own group leader, distinct from ours) must
// actually receive the signal and exit accordingly. Uses a channel rather
// than a sleep to observe the exit (no time.Sleep for synchronization).
func TestKillGroupSignalsARealGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 5")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	pgid := cmd.Process.Pid // Setpgid with no Pgid set makes the child its own group leader.

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	if err := KillGroup(pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("KillGroup(%d, SIGKILL): %v", pgid, err)
	}

	select {
	case <-done:
		// Exited (with a signal-related error, which is expected and not
		// itself a failure) — the important fact is it happened at all.
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit after KillGroup(SIGKILL) — signal did not reach it")
	}
}
