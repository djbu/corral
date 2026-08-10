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

// NotifyProfile is a named, operator-owned subset of globally configured
// delivery backends. It intentionally contains names only: templates never
// carry URLs, tokens, headers, or any new egress destination.
type NotifyProfile struct {
	Name     string
	Backends []string
}

type TemplateRegistry struct {
	Templates      map[string]Template
	NotifyProfiles map[string]NotifyProfile
}

type templatesFile struct {
	Template      []templateFile      `toml:"template"`
	NotifyProfile []notifyProfileFile `toml:"notify_profile"`
}

type templateFile struct {
	Name           string   `toml:"name"`
	Model          string   `toml:"model"`
	EnvPassthrough []string `toml:"env_passthrough"`
	BudgetUSD      *float64 `toml:"budget_usd"`
	IdleTimeout    string   `toml:"idle_timeout"`
	NotifyProfile  string   `toml:"notify_profile"`
}

type notifyProfileFile struct {
	Name     string   `toml:"name"`
	Backends []string `toml:"backends"`
}

var templateNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadTemplates reads the user-owned template registry. A missing file is an
// empty registry, which lets existing installations upgrade without setup.
func LoadTemplates() (map[string]Template, error) {
	registry, err := LoadTemplateRegistry()
	if err != nil {
		return nil, err
	}
	return registry.Templates, nil
}

// LoadTemplateRegistry loads templates and their notification profiles from
// one user-owned file, so references are validated atomically at startup.
func LoadTemplateRegistry() (TemplateRegistry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return TemplateRegistry{}, fmt.Errorf("config: resolving home for templates: %w", err)
	}
	return loadTemplateRegistryFile(filepath.Join(home, ".corral", "templates.toml"))
}

func loadTemplatesFile(path string) (map[string]Template, error) {
	registry, err := loadTemplateRegistryFile(path)
	if err != nil {
		return nil, err
	}
	return registry.Templates, nil
}

func loadTemplateRegistryFile(path string) (TemplateRegistry, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return TemplateRegistry{Templates: map[string]Template{}, NotifyProfiles: map[string]NotifyProfile{}}, nil
		}
		return TemplateRegistry{}, fmt.Errorf("config: stat templates %s: %w", path, err)
	}
	var raw templatesFile
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return TemplateRegistry{}, fmt.Errorf("config: parsing templates %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return TemplateRegistry{}, fmt.Errorf("config: templates %s: unknown key %s", path, undecoded[0])
	}
	profiles := make(map[string]NotifyProfile, len(raw.NotifyProfile))
	for _, in := range raw.NotifyProfile {
		if !templateNameRE.MatchString(in.Name) {
			return TemplateRegistry{}, fmt.Errorf("config: notify profile name %q must match %s", in.Name, templateNameRE)
		}
		if _, exists := profiles[in.Name]; exists {
			return TemplateRegistry{}, fmt.Errorf("config: duplicate notify profile %q", in.Name)
		}
		seen := map[string]bool{}
		for _, backend := range in.Backends {
			if backend != "ntfy" && backend != "webhook" {
				return TemplateRegistry{}, fmt.Errorf("config: notify profile %q: unsupported backend %q", in.Name, backend)
			}
			if seen[backend] {
				return TemplateRegistry{}, fmt.Errorf("config: notify profile %q: duplicate backend %q", in.Name, backend)
			}
			seen[backend] = true
		}
		profiles[in.Name] = NotifyProfile{Name: in.Name, Backends: append([]string(nil), in.Backends...)}
	}
	out := make(map[string]Template, len(raw.Template))
	for _, in := range raw.Template {
		if !templateNameRE.MatchString(in.Name) {
			return TemplateRegistry{}, fmt.Errorf("config: template name %q must match %s", in.Name, templateNameRE)
		}
		if _, exists := out[in.Name]; exists {
			return TemplateRegistry{}, fmt.Errorf("config: duplicate template %q", in.Name)
		}
		for _, env := range in.EnvPassthrough {
			if !envNameRE.MatchString(env) {
				return TemplateRegistry{}, fmt.Errorf("config: template %q: invalid environment name %q", in.Name, env)
			}
		}
		if in.BudgetUSD != nil && (*in.BudgetUSD <= 0 || math.IsNaN(*in.BudgetUSD) || math.IsInf(*in.BudgetUSD, 0)) {
			return TemplateRegistry{}, fmt.Errorf("config: template %q: budget_usd must be positive and finite", in.Name)
		}
		var idle time.Duration
		if in.IdleTimeout != "" {
			idle, err = time.ParseDuration(in.IdleTimeout)
			if err != nil || idle <= 0 {
				return TemplateRegistry{}, fmt.Errorf("config: template %q: idle_timeout must be a positive duration", in.Name)
			}
		}
		if in.NotifyProfile != "" && !templateNameRE.MatchString(in.NotifyProfile) {
			return TemplateRegistry{}, fmt.Errorf("config: template %q: invalid notify_profile %q", in.Name, in.NotifyProfile)
		}
		if in.NotifyProfile != "" {
			if _, ok := profiles[in.NotifyProfile]; !ok {
				return TemplateRegistry{}, fmt.Errorf("config: template %q: unknown notify_profile %q", in.Name, in.NotifyProfile)
			}
		}
		out[in.Name] = Template{in.Name, in.Model, append([]string(nil), in.EnvPassthrough...), in.BudgetUSD, idle, in.NotifyProfile}
	}
	return TemplateRegistry{Templates: out, NotifyProfiles: profiles}, nil
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
