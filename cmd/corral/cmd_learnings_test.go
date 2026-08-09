package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/danielbecerra/corral/internal/api/client"
)

func TestInterspersedFlagArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		valueFlags []string
		want       []string
	}{
		{
			name: "boolean after id",
			args: []string{"learning-id", "--diff", "--json"},
			want: []string{"--diff", "--json", "learning-id"},
		},
		{
			name:       "value after id",
			args:       []string{"learning-id", "--reason", "operator decision", "--json"},
			valueFlags: []string{"reason"},
			want:       []string{"--reason", "operator decision", "--json", "learning-id"},
		},
		{
			name:       "answer documented order",
			args:       []string{"session-name", "--key", "enter"},
			valueFlags: []string{"key"},
			want:       []string{"--key", "enter", "session-name"},
		},
		{
			name:       "equals form",
			args:       []string{"learning-id", "--host=example.test:443", "--json"},
			valueFlags: []string{"host"},
			want:       []string{"--host=example.test:443", "--json", "learning-id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := interspersedFlagArgs(tt.args, tt.valueFlags...); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("interspersedFlagArgs(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

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
