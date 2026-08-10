#!/usr/bin/env python3
"""Attach to a corral session through a real PTY and detach cleanly."""

import os
import pty
import select
import signal
import sys
import time


def fail(message: str, transcript: bytes, pid: int) -> None:
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    sys.stderr.write(f"attach smoke: {message}\n")
    sys.stderr.buffer.write(transcript[-8192:])
    raise SystemExit(1)


def main() -> int:
    if len(sys.argv) not in (3, 4):
        sys.stderr.write("usage: pty-attach-smoke.py <corral-bin> <session> [prompt]\n")
        return 2

    pid, master = pty.fork()
    if pid == 0:
        os.execv(sys.argv[1], [sys.argv[1], "attach", sys.argv[2]])

    transcript = bytearray()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        ready, _, _ = select.select([master], [], [], 0.25)
        if ready:
            try:
                transcript.extend(os.read(master, 65536))
            except OSError:
                pass
            if b"fakeclaude" in transcript.lower():
                break
        done, status = os.waitpid(pid, os.WNOHANG)
        if done:
            fail(f"attach exited before banner (status={status})", transcript, pid)
    else:
        fail("timed out waiting for fakeclaude banner", transcript, pid)

    if len(sys.argv) == 4:
        marker = ("echo: " + sys.argv[3]).encode()
        os.write(master, sys.argv[3].encode() + b"\n")
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and marker not in transcript:
            ready, _, _ = select.select([master], [], [], 0.25)
            if ready:
                try:
                    transcript.extend(os.read(master, 65536))
                except OSError:
                    pass
        if marker not in transcript:
            fail("timed out waiting for prompt to reach fakeclaude", transcript, pid)

    # Defaults are C-\\ followed by d. Both bytes traverse a real PTY.
    os.write(master, b"\x1cd")
    deadline = time.monotonic() + 10
    status = None
    while time.monotonic() < deadline:
        ready, _, _ = select.select([master], [], [], 0.25)
        if ready:
            try:
                transcript.extend(os.read(master, 65536))
            except OSError:
                pass
        done, child_status = os.waitpid(pid, os.WNOHANG)
        if done:
            status = child_status
            break
    os.close(master)
    if status is None:
        fail("timed out waiting for clean detach", transcript, pid)
    if not os.WIFEXITED(status) or os.WEXITSTATUS(status) != 0:
        fail(f"attach failed (status={status})", transcript, pid)
    if b"detached" not in transcript.lower():
        fail("client exited without detach acknowledgement", transcript, pid)

    print("attach smoke: fakeclaude banner observed and PTY detached cleanly")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
