package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/djbu/corral/internal/version"
)

// Attach performs the attach-protocol HTTP upgrade (design doc §5.1) over a
// fresh dial to the daemon's socket and returns the raw connection plus the
// bufio.Reader positioned exactly where the 101 response ended. Frame reads
// must go through that reader, never a fresh one wrapping conn — any bytes
// the daemon writes right after its 101 status line (e.g. Ready, arriving
// before this call even returns) would otherwise be silently lost, the
// same reasoning as the daemon-side hijack in handlers_attach.go.
//
// c's own *http.Client is not used here: net/http.Client has no client-side
// hijack API, so the request is written by hand to a fresh connection
// instead of going through do().
func (c *Client) Attach(ctx context.Context, idOrName string) (net.Conn, *bufio.Reader, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.sockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("client: dialing %s: %w", c.sockPath, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://corral/v1/sessions/"+url.PathEscape(idOrName)+"/attach", nil)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("client: building attach request: %w", err)
	}
	req.Header.Set("Corral-Api-Version", strconv.Itoa(version.APIVersion))
	req.Header.Set("Corral-Client-Version", version.Version)
	req.Header.Set("Upgrade", "corral-attach/1")
	req.Header.Set("Connection", "Upgrade")

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("client: writing attach request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("client: reading attach response: %w", err)
	}
	c.checkDaemonVersion(resp)

	if resp.StatusCode != http.StatusSwitchingProtocols {
		derr := decode(resp, nil)
		conn.Close()
		if derr != nil {
			return nil, nil, derr
		}
		return nil, nil, fmt.Errorf("client: attach: unexpected status %d", resp.StatusCode)
	}

	return conn, br, nil
}
