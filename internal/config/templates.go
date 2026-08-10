package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/BurntSushi/toml"
)

// Template is operator-owned session policy loaded only from
// ~/.corral/templates.toml. It deliberately has no executable path, command,
// secret, or permission-mode field: a template may narrow routine execution,
// never create a new authority channel.
type Template struct {
	Name           string
	Model          string
	EnvPassthrough []string
	BudgetUSD      *float64
	IdleTimeout    time.Duration
	NotifyProfile  string
}

type templatesFile struct {
	Template []templateFile `toml:"template"`
}

type templateFile struct {
	Name           string   `toml:"name"`
	Model          string   `toml:"model"`
	EnvPassthrough []string `toml:"env_passthrough"`
	BudgetUSD      *float64 `toml:"budget_usd"`
	IdleTimeout    string   `toml:"idle_timeout"`
	NotifyProfile  string   `toml:"notify_profile"`
}

var templateNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadTemplates reads the user-owned template registry. A missing file is an
// empty registry, which lets existing installations upgrade without setup.
func LoadTemplates() (map[string]Template, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: resolving home for templates: %w", err)
	}
	return loadTemplatesFile(filepath.Join(home, ".corral", "templates.toml"))
}

func loadTemplatesFile(path string) (map[string]Template, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]Template{}, nil
		}
		return nil, fmt.Errorf("config: stat templates %s: %w", path, err)
	}
	var raw templatesFile
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return nil, fmt.Errorf("config: parsing templates %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config: templates %s: unknown key %s", path, undecoded[0])
	}
	out := make(map[string]Template, len(raw.Template))
	for _, in := range raw.Template {
		if !templateNameRE.MatchString(in.Name) {
			return nil, fmt.Errorf("config: template name %q must match %s", in.Name, templateNameRE)
		}
		if _, exists := out[in.Name]; exists {
			return nil, fmt.Errorf("config: duplicate template %q", in.Name)
		}
		for _, env := range in.EnvPassthrough {
			if !envNameRE.MatchString(env) {
				return nil, fmt.Errorf("config: template %q: invalid environment name %q", in.Name, env)
			}
		}
		if in.BudgetUSD != nil && (*in.BudgetUSD <= 0 || math.IsNaN(*in.BudgetUSD) || math.IsInf(*in.BudgetUSD, 0)) {
			return nil, fmt.Errorf("config: template %q: budget_usd must be positive and finite", in.Name)
		}
		var idle time.Duration
		if in.IdleTimeout != "" {
			idle, err = time.ParseDuration(in.IdleTimeout)
			if err != nil || idle <= 0 {
				return nil, fmt.Errorf("config: template %q: idle_timeout must be a positive duration", in.Name)
			}
		}
		if in.NotifyProfile != "" && !templateNameRE.MatchString(in.NotifyProfile) {
			return nil, fmt.Errorf("config: template %q: invalid notify_profile %q", in.Name, in.NotifyProfile)
		}
		out[in.Name] = Template{in.Name, in.Model, append([]string(nil), in.EnvPassthrough...), in.BudgetUSD, idle, in.NotifyProfile}
	}
	return out, nil
}

// ResolveTemplate looks up a user-authorized template by its stable name.
func ResolveTemplate(name string) (Template, error) {
	templates, err := LoadTemplates()
	if err != nil {
		return Template{}, err
	}
	t, ok := templates[name]
	if !ok {
		return Template{}, fmt.Errorf("config: unknown session template %q", name)
	}
	return t, nil
}
