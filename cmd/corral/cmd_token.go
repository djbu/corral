package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/djbu/corral/internal/api/client"
)

// cmdToken implements `corral token <create|list|revoke>` (m5.md §11 step
// 27): local-admin management of api bearer tokens over the unix socket.
// There is no bearer auth on these endpoints yet (that's step 28/30) —
// minting a token today is itself a local-socket, local-admin operation.
func cmdToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTokenUsage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "create":
		return tokenCreate(args[1:], stdout, stderr)
	case "list":
		return tokenList(args[1:], stdout, stderr)
	case "revoke":
		return tokenRevoke(args[1:], stdout, stderr)
	default:
		printTokenUsage(stderr)
		return exitUsage
	}
}

// printTokenUsage prints the token command's usage summary to w.
func printTokenUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: corral token <create|list|revoke> [flags]")
	fmt.Fprintln(w, "  corral token create --label <name> [--scope admin|session] [--session <id>]")
	fmt.Fprintln(w, "  corral token list")
	fmt.Fprintln(w, "  corral token revoke <id>")
}

// tokenCreate implements `corral token create` (m5.md §11): mints a token
// over POST /v1/tokens and prints the plaintext to stdout exactly once,
// with a one-line stderr warning that it will never be shown again.
func tokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	label := fs.String("label", "", "human-readable label for this token")
	scope := fs.String("scope", "", `token scope: "admin" (default) or "session"`)
	session := fs.String("session", "", "session id (required when --scope=session)")
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral token create --label <name> [--scope admin|session] [--session <id>]")
		return exitUsage
	}
	if *label == "" {
		fmt.Fprintln(stderr, "usage: corral token create --label <name> [--scope admin|session] [--session <id>]")
		return exitUsage
	}

	c, err := newClient(cf, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: token: %v\n", err)
		return exitError
	}

	created, err := c.CreateToken(context.Background(), client.CreateTokenRequest{
		Label:     *label,
		Scope:     *scope,
		SessionID: *session,
	})
	if err != nil {
		fmt.Fprintf(stderr, "corral: token: %v\n", err)
		return exitError
	}

	fmt.Fprintln(stdout, created.Token)
	fmt.Fprintln(stderr, "corral: this token is shown only once — store it now")
	return exitOK
}

// tokenList implements `corral token list`: a tabwriter table of every
// token's metadata (never the plaintext or hash — GET /v1/tokens's wire
// shape structurally has no field for either).
func tokenList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral token list")
		return exitUsage
	}

	c, err := newClient(cf, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: token: %v\n", err)
		return exitError
	}

	tokens, err := c.ListTokens(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "corral: token: %v\n", err)
		return exitError
	}

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tSCOPE\tSESSION\tCREATED\tLAST-USED\tREVOKED")
	for _, tok := range tokens {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			tok.ID, tok.Label, tok.Scope, emptyColumn(tok.SessionID),
			formatTokenMs(tok.CreatedMs), formatTokenMs(tok.LastUsedMs), formatTokenMs(tok.RevokedMs))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "corral: token: %v\n", err)
		return exitError
	}
	return exitOK
}

// tokenRevoke implements `corral token revoke <id>`.
func tokenRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: corral token revoke <id>")
		return exitUsage
	}
	id := fs.Arg(0)

	c, err := newClient(cf, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: token: %v\n", err)
		return exitError
	}

	if err := c.RevokeToken(context.Background(), id); err != nil {
		fmt.Fprintf(stderr, "corral: token: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "revoked %s\n", id)
	return exitOK
}

// emptyColumn renders "" as "-", matching budgetColumn's (cmd_review.go)
// convention for an absent optional column value.
func emptyColumn(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatTokenMs renders a store epoch-millis timestamp for `token list`:
// 0 (NULL in the store, meaning "never" — last_used_ms/revoked_ms — or, for
// created_ms, an impossible value) renders as "-"; anything else renders
// as RFC3339. This is CLI-side wall-clock display, exempt from the
// daemon's no-time rule (cmd_run.go's waitForDAG sets that precedent).
func formatTokenMs(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}
