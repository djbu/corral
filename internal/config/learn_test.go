package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadLearnDefaults(t *testing.T) {
	withHome(t)

	got, sources, err := LoadLearn()
	if err != nil {
		t.Fatalf("LoadLearn: %v", err)
	}
	if got.Window != 14*24*time.Hour || got.MinApprovals != 3 || got.TTL != 90*24*time.Hour {
		t.Fatalf("unexpected learning defaults: %+v", got)
	}
	if got.MinSessions != 5 || got.MinTerminalTasks != 3 || got.CostRegressionTolerance != 0.10 {
		t.Fatalf("unexpected learning gates: %+v", got)
	}
	for _, key := range []string{
		"learn.window", "learn.min_approvals", "learn.ttl", "learn.min_sessions",
		"learn.min_terminal_tasks", "learn.cost_regression_tolerance",
	} {
		if sources[key] != sourceDefault {
			t.Errorf("source %s = %q, want default", key, sources[key])
		}
	}
}

func TestLoadLearnUserAndEnvPrecedence(t *testing.T) {
	home := withHome(t)
	userPath := filepath.Join(home, ".corral", "config.toml")
	writeFile(t, userPath, ""+
		"[learn]\n"+
		"window = \"168h\"\n"+
		"min_approvals = 4\n"+
		"ttl = \"720h\"\n"+
		"min_sessions = 6\n"+
		"min_terminal_tasks = 5\n"+
		"cost_regression_tolerance = 0.2\n")

	got, sources, err := LoadLearn()
	if err != nil {
		t.Fatalf("LoadLearn user: %v", err)
	}
	if got.MinApprovals != 4 || got.MinSessions != 6 || got.CostRegressionTolerance != 0.2 {
		t.Fatalf("user values not applied: %+v", got)
	}
	if sources["learn.min_approvals"] != userPath {
		t.Fatalf("user source = %q, want %q", sources["learn.min_approvals"], userPath)
	}

	t.Setenv("CORRAL_LEARN_MIN_APPROVALS", "7")
	t.Setenv("CORRAL_LEARN_WINDOW", "48h")
	t.Setenv("CORRAL_LEARN_COST_REGRESSION_TOLERANCE", "0.05")
	got, sources, err = LoadLearn()
	if err != nil {
		t.Fatalf("LoadLearn env: %v", err)
	}
	if got.MinApprovals != 7 || got.Window != 48*time.Hour || got.CostRegressionTolerance != 0.05 {
		t.Fatalf("env values not applied: %+v", got)
	}
	if sources["learn.min_approvals"] != "env CORRAL_LEARN_MIN_APPROVALS" {
		t.Fatalf("env source = %q", sources["learn.min_approvals"])
	}
}

func TestLoadLearnRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		env  string
		val  string
		key  string
	}{
		{"bad duration", "CORRAL_LEARN_WINDOW", "soon", "learn.window"},
		{"zero approvals", "CORRAL_LEARN_MIN_APPROVALS", "0", "learn.min_approvals"},
		{"negative tolerance", "CORRAL_LEARN_COST_REGRESSION_TOLERANCE", "-0.1", "learn.cost_regression_tolerance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withHome(t)
			t.Setenv(tt.env, tt.val)
			_, _, err := LoadLearn()
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("error = %v, want key %s", err, tt.key)
			}
		})
	}
}

func TestLoadSessionRejectsRepoLearnPolicy(t *testing.T) {
	withHome(t)
	_, sub := setupRepo(t)
	repoPath := filepath.Join(sub, ".corral.toml")
	writeFile(t, repoPath, ""+
		"[learn]\n"+
		"window = \"1h\"\n"+
		"min_approvals = 1\n"+
		"ttl = \"1h\"\n"+
		"min_sessions = 1\n"+
		"min_terminal_tasks = 1\n"+
		"cost_regression_tolerance = 9.0\n")

	_, _, _, rejections, err := LoadSession(sub, nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	want := []string{
		"learn.window", "learn.min_approvals", "learn.ttl", "learn.min_sessions",
		"learn.min_terminal_tasks", "learn.cost_regression_tolerance",
	}
	for _, key := range want {
		assertRejected(t, rejections, repoPath, key)
	}
	if len(rejections) != len(want) {
		t.Fatalf("rejections = %+v, want exactly %v", rejections, want)
	}
}

func TestEffectiveIncludesLearnPolicy(t *testing.T) {
	withHome(t)
	eff, err := Effective(t.TempDir())
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if eff.Values["learn.min_approvals"] != "3" || eff.Values["learn.window"] != "336h0m0s" {
		t.Fatalf("effective learning values = %+v", eff.Values)
	}
	if eff.Sources["learn.cost_regression_tolerance"] != sourceDefault {
		t.Fatalf("learning source = %q", eff.Sources["learn.cost_regression_tolerance"])
	}
}
