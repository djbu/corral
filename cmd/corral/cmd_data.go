package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/daemon"
	"github.com/djbu/corral/internal/dataops"
	"github.com/djbu/corral/internal/store"
)

func cmdBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	output := fs.String("output", "", "absolute snapshot path (default: <state_dir>/backups/<timestamp>.db)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral backup [--output <absolute-path>]")
		return exitUsage
	}
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: backup: %v\n", err)
		return exitError
	}
	dest := *output
	if dest == "" {
		dest = filepath.Join(cfg.StateDir, "backups", "corral-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
	}
	if !filepath.IsAbs(dest) {
		fmt.Fprintln(stderr, "corral: backup: --output must be an absolute path")
		return exitUsage
	}
	source := filepath.Join(cfg.StateDir, "corral.db")
	info, err := store.BackupDatabase(context.Background(), source, dest)
	if err != nil {
		fmt.Fprintf(stderr, "corral: backup: %v\n", err)
		return exitError
	}
	st, err := os.Stat(dest)
	if err != nil {
		fmt.Fprintf(stderr, "corral: backup: stat result: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "backup created path=%s bytes=%d schema=%d integrity=ok\n", dest, st.Size(), info.SchemaVersion)
	return exitOK
}

func cmdGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report candidates without mutation")
	apply := fs.Bool("apply", false, "remove exact candidates and perform maintenance")
	olderThan := fs.Duration("older-than", 30*24*time.Hour, "always select orphan session directories older than this")
	maxBytesRaw := fs.String("max-bytes", "1GiB", "maximum retained orphan-session bytes")
	vacuum := fs.Bool("vacuum", false, "VACUUM only when reclaimable bytes meet the threshold")
	vacuumMinRaw := fs.String("vacuum-min-reclaim", "64MiB", "minimum freelist bytes required for VACUUM")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 || *dryRun == *apply || *olderThan < 0 {
		fmt.Fprintln(stderr, "usage: corral gc (--dry-run|--apply) [--older-than D] [--max-bytes N] [--vacuum]")
		return exitUsage
	}
	maxBytes, err := config.ParseBytes(*maxBytesRaw)
	if err != nil || maxBytes < 0 {
		fmt.Fprintf(stderr, "corral: gc: invalid --max-bytes %q: %v\n", *maxBytesRaw, err)
		return exitUsage
	}
	vacuumMin, err := config.ParseBytes(*vacuumMinRaw)
	if err != nil || vacuumMin < 0 {
		fmt.Fprintf(stderr, "corral: gc: invalid --vacuum-min-reclaim %q: %v\n", *vacuumMinRaw, err)
		return exitUsage
	}
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: gc: %v\n", err)
		return exitError
	}
	status, err := inspectDaemonLifecycle(cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: gc: inspecting daemon: %v\n", err)
		return exitError
	}
	if status.State != daemon.LifecycleStopped {
		fmt.Fprintf(stderr, "corral: gc: daemon must be cleanly stopped; current state=%s\n", status.State)
		return exitError
	}
	if *apply {
		lock, err := daemon.AcquireLock(cfg.StateDir)
		if err != nil {
			fmt.Fprintf(stderr, "corral: gc: acquiring maintenance lock: %v\n", err)
			return exitError
		}
		defer lock.Close()
	}
	ctx := context.Background()
	dbPath := filepath.Join(cfg.StateDir, "corral.db")
	ids, err := store.KnownSessionIDsFromDatabase(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "corral: gc: %v\n", err)
		return exitError
	}
	plan, err := dataops.PlanGC(cfg.StateDir, ids, time.Now(), *olderThan, maxBytes)
	if err != nil {
		fmt.Fprintf(stderr, "corral: gc: %v\n", err)
		return exitError
	}
	var selectedBytes int64
	for _, candidate := range plan.Candidates {
		selectedBytes += candidate.Bytes
		fmt.Fprintf(stdout, "%s path=%s bytes=%d reason=%s\n", map[bool]string{true: "would-remove", false: "remove"}[*dryRun], candidate.Path, candidate.Bytes, candidate.Reason)
	}
	if *dryRun {
		fmt.Fprintf(stdout, "gc dry-run candidates=%d bytes=%d orphan_bytes=%d\n", len(plan.Candidates), selectedBytes, plan.OrphanBytes)
		return exitOK
	}
	st, err := store.Open(dbPath, clock.Real())
	if err != nil {
		fmt.Fprintf(stderr, "corral: gc: opening store: %v\n", err)
		return exitError
	}
	maintenance, maintainErr := st.Maintain(ctx, *vacuum, vacuumMin)
	closeErr := st.Close()
	if maintainErr != nil {
		fmt.Fprintf(stderr, "corral: gc: maintenance: %v\n", maintainErr)
		return exitError
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "corral: gc: close store: %v\n", closeErr)
		return exitError
	}
	removed, err := dataops.ApplyGC(cfg.StateDir, plan.Candidates)
	if err != nil {
		fmt.Fprintf(stderr, "corral: gc: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "gc complete removed=%d bytes=%d reclaimable_db_bytes=%d vacuumed=%t\n", len(plan.Candidates), removed, maintenance.ReclaimableBytes, maintenance.Vacuumed)
	return exitOK
}

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	replace := fs.Bool("replace", false, "replace an existing database after creating a rollback snapshot")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: corral restore [--replace] <absolute-backup.db>")
		return exitUsage
	}
	backup := fs.Arg(0)
	if !filepath.IsAbs(backup) {
		fmt.Fprintln(stderr, "corral: restore: backup path must be absolute")
		return exitUsage
	}
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: restore: %v\n", err)
		return exitError
	}
	status, err := inspectDaemonLifecycle(cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: restore: inspecting daemon: %v\n", err)
		return exitError
	}
	if status.State != daemon.LifecycleStopped {
		fmt.Fprintf(stderr, "corral: restore: daemon must be cleanly stopped; current state=%s\n", status.State)
		return exitError
	}
	lock, err := daemon.AcquireLock(cfg.StateDir)
	if err != nil {
		fmt.Fprintf(stderr, "corral: restore: acquiring maintenance lock: %v\n", err)
		return exitError
	}
	defer lock.Close()
	target := filepath.Join(cfg.StateDir, "corral.db")
	rollback := ""
	if _, err := os.Lstat(target); err == nil {
		if !*replace {
			fmt.Fprintln(stderr, "corral: restore: target exists; pass --replace to create a rollback snapshot and continue")
			return exitError
		}
		rollback = filepath.Join(cfg.StateDir, "backups", "pre-restore-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
		if _, err := store.BackupDatabase(context.Background(), target, rollback); err != nil {
			fmt.Fprintf(stderr, "corral: restore: creating rollback snapshot: %v\n", err)
			return exitError
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "corral: restore: stat target: %v\n", err)
		return exitError
	}
	info, err := store.RestoreDatabase(context.Background(), backup, target)
	if err != nil {
		fmt.Fprintf(stderr, "corral: restore: %v", err)
		if rollback != "" {
			fmt.Fprintf(stderr, " (current state preserved at %s)", rollback)
		}
		fmt.Fprintln(stderr)
		return exitError
	}
	fmt.Fprintf(stdout, "database restored path=%s schema=%d integrity=ok", target, info.SchemaVersion)
	if rollback != "" {
		fmt.Fprintf(stdout, " rollback=%s", rollback)
	}
	fmt.Fprintln(stdout)
	return exitOK
}
