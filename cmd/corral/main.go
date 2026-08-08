// Command corral is the CLI entry point for the corral daemon and its
// client subcommands. main itself only builds the dispatch table and hands
// off to run; run is unit-testable without process-level side effects
// (os.Exit, real stdout/stderr) by taking those as parameters.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
)

// Exit codes. 0/1/2 follow the conventional success/failure/usage-error
// split; they are part of the CLI's contract with scripts and CI, so keep
// them stable.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// cmdFunc is the signature every subcommand implements: given its own argv
// (not including the subcommand name) and the stdout/stderr streams to write
// to, it returns a process exit code.
type cmdFunc func(args []string, stdout, stderr io.Writer) int

// commands is the dispatch table from subcommand name to implementation.
var commands = map[string]cmdFunc{
	"config":     cmdConfig,
	"daemon":     cmdDaemon,
	"daemon-run": cmdDaemonRun,
	"doctor":     cmdDoctor,
	"ls":         cmdLs,
	"new":        cmdNew,
	"kill":       cmdKill,
	"answer":     cmdAnswer,
	"attach":     cmdAttach,
	"hook-relay": cmdHookRelay,
}

// hiddenCommands are dispatchable but never listed by printCommands.
// "daemon-run" is the detached daemon body (design doc §3.1); it is always
// launched by `corral daemon` itself via re-exec, never meant to be typed
// by a human. "hook-relay" is likewise always launched by the daemon's
// generated hooks{} settings block (design doc §2.4), never by a human.
var hiddenCommands = map[string]bool{
	"daemon-run": true,
	"hook-relay": true,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches args[0] to the matching subcommand in commands, handling
// the top-level --version/--help flags first. It never calls os.Exit itself
// so tests can assert on returned codes and captured output.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: corral <command> [flags]")
		printCommands(stderr)
		return exitUsage
	}

	switch args[0] {
	case "--version", "-version":
		fmt.Fprintln(stdout, versionString())
		return exitOK
	case "--help", "-h", "help":
		fmt.Fprintln(stdout, "usage: corral <command> [flags]")
		printCommands(stdout)
		return exitOK
	}

	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "corral: unknown command %q\n", args[0])
		printCommands(stderr)
		return exitUsage
	}
	return cmd(args[1:], stdout, stderr)
}

func printCommands(w io.Writer) {
	names := make([]string, 0, len(commands))
	for name := range commands {
		if hiddenCommands[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintln(w, "commands:")
	for _, name := range names {
		fmt.Fprintf(w, "  %s\n", name)
	}
}
