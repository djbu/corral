package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/danielbecerra/corral/internal/api/client"
)

func TestCmdLearningsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdLearnings(nil, &stdout, &stderr); code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "scan|list|show|report") {
		t.Fatalf("usage = %q", stderr.String())
	}
}

func TestLearningRule(t *testing.T) {
	got := learningRule(client.LearningInfo{Content: json.RawMessage(`{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}`)})
	if got != "Bash(npm test)" {
		t.Fatalf("learningRule = %q", got)
	}
	if got := learningRule(client.LearningInfo{Content: json.RawMessage(`{}`)}); got != "-" {
		t.Fatalf("empty learningRule = %q", got)
	}
}

func TestPrintLearningJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := printLearningJSON(&stdout, &stderr, map[string]int{"count": 1}); code != exitOK {
		t.Fatalf("code = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"count": 1`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
