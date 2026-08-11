package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/djbu/corral/internal/api/client"
)

func TestMCPInitializeAndToolCatalog(t *testing.T) {
	init := handleMCP(context.Background(), nil, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"})
	if init.Error != nil {
		t.Fatalf("initialize error = %+v", init.Error)
	}
	result, ok := init.Result.(map[string]any)
	if !ok || result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("initialize result = %#v", init.Result)
	}
	tools := handleMCP(context.Background(), nil, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/list"})
	if tools.Error != nil {
		t.Fatalf("tools/list error = %+v", tools.Error)
	}
	b, _ := json.Marshal(tools.Result)
	for _, want := range []string{"corral_status", "corral_run", "corral_wait"} {
		if !containsJSONName(b, want) {
			t.Fatalf("catalog %s missing %q", b, want)
		}
	}
}

func TestMCPStatusUsesRealDaemonClientContract(t *testing.T) {
	file, err := os.CreateTemp("/tmp", "corral-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	sock := file.Name() + ".sock"
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/sessions":
			_, _ = w.Write([]byte(`{"sessions":[{"id":"s1","name":"one"}]}`))
		case "/v1/dags":
			_, _ = w.Write([]byte(`{"dags":[{"dag_id":"d1","cost_usd":0}]}`))
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"corral_status","arguments":{}}}` + "\n"
	var out bytes.Buffer
	if got := serveMCP(strings.NewReader(request), &out, client.New(sock, nil)); got != exitOK {
		t.Fatalf("serveMCP exit = %d", got)
	}
	if !strings.Contains(out.String(), `\"sessions\"`) || !strings.Contains(out.String(), `\"dags\"`) {
		t.Fatalf("MCP response = %s", out.String())
	}
}

func TestMCPUnknownToolCannotEscalate(t *testing.T) {
	_, err := callMCPTool(context.Background(), nil, "corral_token_create", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("unknown MCP tool accepted")
	}
}

func containsJSONName(b []byte, want string) bool { return strings.Contains(string(b), want) }
