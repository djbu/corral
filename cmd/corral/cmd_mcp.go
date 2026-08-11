package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/djbu/corral/internal/api/client"
)

// cmdMCP serves the Model Context Protocol over stdio. It deliberately owns
// no token, socket, or authorization logic: newClient is the same choke point
// used by ordinary commands, so scoped credentials remain scoped at the API.
func cmdMCP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral mcp [--host host:port]")
		return exitUsage
	}
	c, err := newClient(cf, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: mcp: %v\n", err)
		return exitError
	}
	return serveMCP(os.Stdin, stdout, c)
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}
type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serveMCP(in io.Reader, out io.Writer, c *client.Client) int {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), 1<<20)
	enc := json.NewEncoder(out)
	for scan.Scan() {
		var req mcpRequest
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			_ = enc.Encode(mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: -32700, Message: "parse error"}})
			continue
		}
		resp := handleMCP(context.Background(), c, req)
		if len(req.ID) != 0 {
			if err := enc.Encode(resp); err != nil {
				return exitError
			}
		}
	}
	if err := scan.Err(); err != nil {
		return exitError
	}
	return exitOK
}

func handleMCP(ctx context.Context, c *client.Client, req mcpRequest) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "corral", "version": "0.9.0-dev"}}
	case "tools/list":
		resp.Result = map[string]any{"tools": []map[string]any{
			{"name": "corral_status", "description": "List sessions and DAG summaries visible to the configured Corral credential.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			{"name": "corral_run", "description": "Submit one headless task through Corral's existing DAG admission and budget checks.", "inputSchema": map[string]any{"type": "object", "required": []string{"prompt", "repo"}, "properties": map[string]any{"prompt": map[string]string{"type": "string"}, "repo": map[string]string{"type": "string"}, "model": map[string]string{"type": "string"}, "template": map[string]string{"type": "string"}}}},
			{"name": "corral_wait", "description": "Read a DAG until all tasks are terminal or timeout expires.", "inputSchema": map[string]any{"type": "object", "required": []string{"dag_id"}, "properties": map[string]any{"dag_id": map[string]string{"type": "string"}, "timeout_seconds": map[string]string{"type": "integer"}}}},
		}}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mcpFailure(resp, "invalid tool request")
		}
		result, err := callMCPTool(ctx, c, p.Name, p.Arguments)
		if err != nil {
			return mcpFailure(resp, err.Error())
		}
		b, _ := json.Marshal(result)
		resp.Result = map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}
	default:
		return mcpFailure(resp, "method not found")
	}
	return resp
}

func mcpFailure(r mcpResponse, message string) mcpResponse {
	r.Error = &mcpError{Code: -32602, Message: message}
	return r
}

func callMCPTool(ctx context.Context, c *client.Client, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "corral_status":
		s, e := c.ListSessions(ctx)
		if e != nil {
			return nil, e
		}
		d, e := c.ListDAGs(ctx)
		return map[string]any{"sessions": s, "dags": d}, e
	case "corral_run":
		var a struct{ Prompt, Repo, Model, Template string }
		if e := json.Unmarshal(raw, &a); e != nil || a.Prompt == "" || a.Repo == "" {
			return nil, fmt.Errorf("prompt and repo are required")
		}
		return c.SubmitDAG(ctx, client.SubmitDagRequest{Nodes: []client.DagNode{{Name: "mcp", Prompt: a.Prompt, Repo: a.Repo, Model: a.Model, Template: a.Template}}})
	case "corral_wait":
		var a struct {
			DAGID          string `json:"dag_id"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if e := json.Unmarshal(raw, &a); e != nil || a.DAGID == "" {
			return nil, fmt.Errorf("dag_id is required")
		}
		if a.TimeoutSeconds <= 0 {
			a.TimeoutSeconds = 300
		}
		deadline := time.Now().Add(time.Duration(a.TimeoutSeconds) * time.Second)
		for {
			d, e := c.GetDAG(ctx, a.DAGID)
			if e != nil {
				return nil, e
			}
			done := true
			for _, t := range d.Tasks {
				if t.Status != "succeeded" && t.Status != "failed" && t.Status != "cancelled" {
					done = false
				}
			}
			if done || time.Now().After(deadline) {
				return d, nil
			}
			time.Sleep(time.Second)
		}
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
