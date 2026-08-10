// Package service manages corral as a per-user launchd or systemd service.
// It deliberately owns only the service descriptor; state, config, logs, and
// the installed binary remain untouched by uninstall.
package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	PlatformDarwin = "darwin"
	PlatformLinux  = "linux"

	launchdLabel = "com.djbu.corral"
	systemdUnit  = "corral.service"
)

// Runner is the narrow external-command seam used by launchctl/systemctl.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Config is fully resolved by the CLI before constructing a Manager.
type Config struct {
	Platform   string
	UID        int
	HomeDir    string
	StateDir   string
	BinaryPath string
	// BinaryDigest makes an in-place binary upgrade observable even when its
	// absolute path is unchanged, so install can restart the supervised job.
	BinaryDigest string
}

type Manager struct {
	cfg    Config
	runner Runner
}

// Status is stable CLI JSON for service status.
type Status struct {
	Platform  string `json:"platform"`
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Active    bool   `json:"active"`
	Detail    string `json:"detail,omitempty"`
}

func New(cfg Config, runner Runner) (*Manager, error) {
	if cfg.Platform != PlatformDarwin && cfg.Platform != PlatformLinux {
		return nil, fmt.Errorf("service: unsupported platform %q (supported: darwin, linux)", cfg.Platform)
	}
	if cfg.UID == 0 {
		return nil, errors.New("service: refusing to manage a user service as root")
	}
	if runner == nil {
		return nil, errors.New("service: command runner is required")
	}
	for name, value := range map[string]string{"home": cfg.HomeDir, "state_dir": cfg.StateDir, "binary": cfg.BinaryPath} {
		if !filepath.IsAbs(value) {
			return nil, fmt.Errorf("service: %s path must be absolute: %q", name, value)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("service: %s path contains a control character", name)
		}
		if filepath.Clean(value) != value {
			return nil, fmt.Errorf("service: %s path must be clean: %q", name, value)
		}
	}
	decodedDigest, err := hex.DecodeString(cfg.BinaryDigest)
	if err != nil || len(decodedDigest) != sha256Size || cfg.BinaryDigest != strings.ToLower(cfg.BinaryDigest) {
		return nil, errors.New("service: binary digest must be 64 lowercase hexadecimal characters")
	}
	return &Manager{cfg: cfg, runner: runner}, nil
}

const sha256Size = 32

func (m *Manager) Path() string {
	if m.cfg.Platform == PlatformDarwin {
		return filepath.Join(m.cfg.HomeDir, "Library", "LaunchAgents", launchdLabel+".plist")
	}
	return filepath.Join(m.cfg.HomeDir, ".config", "systemd", "user", systemdUnit)
}

func (m *Manager) Render() ([]byte, error) {
	if m.cfg.Platform == PlatformDarwin {
		return renderLaunchd(m.cfg)
	}
	return renderSystemd(m.cfg)
}

func (m *Manager) Install(ctx context.Context) (Status, bool, error) {
	// launchd opens StandardOutPath/StandardErrorPath before exec, so the
	// daemon cannot create this directory for us. Creating it here also keeps
	// the service contract identical on systemd and preserves daemon's 0700
	// state-directory boundary.
	if err := ensureStateDir(m.cfg.StateDir); err != nil {
		return Status{}, false, err
	}
	content, err := m.Render()
	if err != nil {
		return Status{}, false, err
	}
	path := m.Path()
	existing, installed, err := readDescriptor(path)
	if err != nil {
		return Status{}, false, err
	}
	changed := !installed || !bytes.Equal(existing, content)
	before, err := m.Status(ctx)
	if err != nil {
		return Status{}, false, err
	}

	if changed && m.cfg.Platform == PlatformDarwin && before.Enabled {
		if _, err := m.runner.Run(ctx, "launchctl", "bootout", m.launchdTarget()); err != nil {
			return Status{}, false, err
		}
	}
	if changed {
		if err := writeDescriptor(path, content); err != nil {
			return Status{}, false, err
		}
	}

	if m.cfg.Platform == PlatformDarwin {
		if changed || !before.Enabled {
			if _, err := m.runner.Run(ctx, "launchctl", "bootstrap", m.launchdDomain(), path); err != nil {
				return Status{}, false, err
			}
		} else if !before.Active {
			if _, err := m.runner.Run(ctx, "launchctl", "kickstart", "-k", m.launchdTarget()); err != nil {
				return Status{}, false, err
			}
		}
	} else {
		if changed {
			if _, err := m.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
				return Status{}, false, err
			}
		}
		if !before.Enabled || !before.Active {
			if _, err := m.runner.Run(ctx, "systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
				return Status{}, false, err
			}
		} else if changed {
			if _, err := m.runner.Run(ctx, "systemctl", "--user", "restart", systemdUnit); err != nil {
				return Status{}, false, err
			}
		}
	}

	status, err := m.Status(ctx)
	return status, changed, err
}

func ensureStateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("service: creating state directory %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("service: restricting state directory %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("service: stat state directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("service: state path is not a directory: %s", path)
	}
	return nil
}

func (m *Manager) Status(ctx context.Context) (Status, error) {
	path := m.Path()
	_, installed, err := readDescriptor(path)
	if err != nil {
		return Status{}, err
	}
	status := Status{Platform: m.cfg.Platform, Path: path, Installed: installed}
	if m.cfg.Platform == PlatformDarwin {
		out, runErr := m.runner.Run(ctx, "launchctl", "print", m.launchdTarget())
		if managerUnavailable(runErr) {
			return Status{}, fmt.Errorf("service: launchctl is unavailable: %w", runErr)
		}
		if managerSessionUnavailable(runErr) {
			return Status{}, fmt.Errorf("service: launchd user domain is unavailable: %w", runErr)
		}
		if runErr == nil {
			status.Enabled = true
			status.Active = launchdRunning(out)
			status.Detail = launchdState(out)
		}
		return status, nil
	}

	if _, runErr := m.runner.Run(ctx, "systemctl", "--user", "is-enabled", "--quiet", systemdUnit); runErr == nil {
		status.Enabled = true
	} else if managerUnavailable(runErr) {
		return Status{}, fmt.Errorf("service: systemctl is unavailable: %w", runErr)
	} else if managerSessionUnavailable(runErr) {
		return Status{}, fmt.Errorf("service: systemd user manager is unavailable: %w", runErr)
	}
	if _, runErr := m.runner.Run(ctx, "systemctl", "--user", "is-active", "--quiet", systemdUnit); runErr == nil {
		status.Active = true
	} else if managerUnavailable(runErr) {
		return Status{}, fmt.Errorf("service: systemctl is unavailable: %w", runErr)
	} else if managerSessionUnavailable(runErr) {
		return Status{}, fmt.Errorf("service: systemd user manager is unavailable: %w", runErr)
	}
	return status, nil
}

func managerSessionUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"failed to connect to bus", "no medium found", "could not find domain", "domain does not exist"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func managerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	return errors.As(err, &execErr)
}

func (m *Manager) Uninstall(ctx context.Context) (bool, error) {
	path := m.Path()
	_, installed, err := readDescriptor(path)
	if err != nil {
		return false, err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return false, err
	}
	if m.cfg.Platform == PlatformDarwin {
		if status.Enabled {
			if _, err := m.runner.Run(ctx, "launchctl", "bootout", m.launchdTarget()); err != nil {
				return false, err
			}
		}
	} else {
		if status.Enabled || status.Active {
			if _, err := m.runner.Run(ctx, "systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
				return false, err
			}
		}
	}
	if installed {
		if err := os.Remove(path); err != nil {
			return false, fmt.Errorf("service: removing %s: %w", path, err)
		}
	}
	if m.cfg.Platform == PlatformLinux && installed {
		if _, err := m.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return false, err
		}
	}
	return installed || status.Enabled || status.Active, nil
}

func (m *Manager) launchdDomain() string {
	return "gui/" + strconv.Itoa(m.cfg.UID)
}

func (m *Manager) launchdTarget() string {
	return m.launchdDomain() + "/" + launchdLabel
}

func renderLaunchd(cfg Config) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("  <!-- corral-binary-sha256: " + cfg.BinaryDigest + " -->\n")
	writePlistKeyString(&b, "Label", launchdLabel)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range []string{cfg.BinaryPath, "daemon", "--foreground"} {
		b.WriteString("    <string>")
		if err := xml.EscapeText(&b, []byte(arg)); err != nil {
			return nil, err
		}
		b.WriteString("</string>\n")
	}
	b.WriteString("  </array>\n")
	writePlistKeyString(&b, "WorkingDirectory", cfg.HomeDir)
	writePlistKeyString(&b, "StandardOutPath", filepath.Join(cfg.StateDir, "service.log"))
	writePlistKeyString(&b, "StandardErrorPath", filepath.Join(cfg.StateDir, "service.log"))
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>\n")
	b.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func writePlistKeyString(b *bytes.Buffer, key, value string) {
	fmt.Fprintf(b, "  <key>%s</key>\n  <string>", key)
	_ = xml.EscapeText(b, []byte(value))
	b.WriteString("</string>\n")
}

func renderSystemd(cfg Config) ([]byte, error) {
	binary, err := systemdExecQuote(cfg.BinaryPath)
	if err != nil {
		return nil, err
	}
	home, err := systemdPathQuote(cfg.HomeDir)
	if err != nil {
		return nil, err
	}
	text := "[Unit]\n" +
		"# corral-binary-sha256: " + cfg.BinaryDigest + "\n" +
		"Description=corral user daemon\n" +
		"After=default.target\n\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=" + binary + " daemon --foreground\n" +
		"WorkingDirectory=" + home + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=2\n" +
		"UMask=0077\n\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
	return []byte(text), nil
}

func systemdExecQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("service: systemd value contains a control character")
	}
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$")
	return "\"" + replacer.Replace(value) + "\"", nil
}

// systemdPathQuote is for path-valued unit settings such as
// WorkingDirectory. Unlike ExecStart, those settings do not perform $VAR
// expansion, so '$' must remain literal; '%' is still a specifier and must be
// doubled.
func systemdPathQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("service: systemd value contains a control character")
	}
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%")
	return "\"" + replacer.Replace(value) + "\"", nil
}

func readDescriptor(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("service: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("service: refusing non-regular descriptor %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("service: reading %s: %w", path, err)
	}
	return data, true, nil
}

func writeDescriptor(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("service: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".corral-service-")
	if err != nil {
		return fmt.Errorf("service: creating temporary descriptor: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("service: replacing %s: %w", path, err)
	}
	d, err := os.Open(dir)
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func launchdRunning(out []byte) bool {
	return launchdState(out) == "running"
}

func launchdState(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "state = "); ok {
			return strings.TrimSpace(value)
		}
	}
	return "loaded"
}
