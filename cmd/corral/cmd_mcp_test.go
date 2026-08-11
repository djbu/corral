package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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

func TestMCPUnknownToolCannotEscalate(t *testing.T) {
	_, err := callMCPTool(context.Background(), nil, "corral_token_create", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("unknown MCP tool accepted")
	}
}

func containsJSONName(b []byte, want string) bool { return strings.Contains(string(b), want) }
