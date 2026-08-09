package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/djbu/corral/internal/api/client"
	"github.com/djbu/corral/internal/config"
)

// clientFlags holds the one shared remote-targeting flag every
// daemon-talking subcommand registers. --host is the ONLY flag: the bearer
// token and CA-cert path are deliberately NOT flags, so a plaintext token
// never lands in `ps aux` or shell history — they come only from
// CORRAL_CLIENT_TOKEN / CORRAL_CLIENT_CACERT or the 0600 ~/.corral/
// config.toml [client] table (design doc m5.md §7 rule 1).
type clientFlags struct {
	host *string
}

// addClientFlags registers --host on fs. Call it before fs.Parse.
func addClientFlags(fs *flag.FlagSet) *clientFlags {
	return &clientFlags{
		host: fs.String("host", "", "target a remote daemon (host:port) over TLS instead of the local unix socket"),
	}
}

// resolveTarget resolves the effective daemon target: the --host flag wins
// over client.host from env/config, and "" means the local unix socket
// (design doc m5.md §7 rule 3: no host resolved must never fall back to
// dialing anything other than the local socket — there is no implicit
// remote default). cf may be nil (some callers, like tests exercising
// resolveTarget directly, have no flag set to consult).
func resolveTarget(cf *clientFlags) (host string, daemon config.Daemon, cl config.Client, err error) {
	daemon, _, err = config.LoadDaemon()
	if err != nil {
		return "", config.Daemon{}, config.Client{}, err
	}
	cl, _, err = config.LoadClient()
	if err != nil {
		return "", config.Daemon{}, config.Client{}, err
	}
	host = cl.Host
	if cf != nil && *cf.host != "" {
		host = *cf.host
	}
	return host, daemon, cl, nil
}

// newClient builds the SDK client for a management command: a remote (TLS +
// bearer) client when a host is resolved, else the local unix-socket
// client. This is the single choke-point every daemon-talking subcommand
// (other than attach, which uses newLocalClient below) goes through, so the
// local-vs-remote decision is made exactly once per invocation.
func newClient(cf *clientFlags, stderr io.Writer) (*client.Client, error) {
	host, daemon, cl, err := resolveTarget(cf)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return client.New(daemon.Socket, stderr), nil
	}
	return client.NewRemote(host, cl.Token, cl.CACert, stderr)
}

// newLocalClient is newClient for commands that can only ever run against a
// local daemon (attach's PTY hijack has no remote transport — design doc
// m5.md §7 rule 3). If a remote target is resolved, it fails loudly rather
// than silently dialing a local socket the operator may not have intended.
func newLocalClient(cf *clientFlags, stderr io.Writer) (*client.Client, error) {
	host, daemon, _, err := resolveTarget(cf)
	if err != nil {
		return nil, err
	}
	if host != "" {
		return nil, fmt.Errorf("attach requires a local daemon; %q is remote (use the web dashboard to manage remote sessions)", host)
	}
	return client.New(daemon.Socket, stderr), nil
}
