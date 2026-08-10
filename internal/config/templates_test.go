package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTemplatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.toml")
	if err := os.WriteFile(path, []byte(`[[template]]
name = "review"
model = "sonnet"
env_passthrough = ["GITHUB_TOKEN"]
budget_usd = 2.5
idle_timeout = "15m"
notify_profile = "team"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadTemplatesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tpl := got["review"]
	if tpl.Model != "sonnet" || tpl.BudgetUSD == nil || *tpl.BudgetUSD != 2.5 || tpl.IdleTimeout.String() != "15m0s" || tpl.NotifyProfile != "team" {
		t.Fatalf("template = %+v", tpl)
	}
}

func TestLoadTemplatesFileRejectsUnsafeFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "templates.toml")
	if err := os.WriteFile(path, []byte("[[template]]\nname = \"x\"\nclaude_bin = \"/tmp/evil\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTemplatesFile(path); err == nil {
		t.Fatal("unsafe unknown field accepted")
	}
}
