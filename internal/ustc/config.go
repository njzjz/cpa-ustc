// Package ustc implements USTC TokenWorks authentication and campus networking.
package ustc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	Provider              = "ustc"
	Version               = "0.1.2"
	Issuer                = "https://id.ustc.edu.cn/oidc"
	BootstrapURL          = "https://id.ustc.edu.cn/ai/api/v1/bootstrap"
	UsageURL              = "https://id.ustc.edu.cn/ai/api/v1/usage"
	APIBase               = "https://api.llm.ustc.edu.cn/v1"
	CampusCheckURL        = "https://id.ustc.edu.cn/doc/claude/netcheck"
	DefaultBudgetCooldown = "30m"
)

// Config contains only public settings. Personal keys live in CPA's auth files.
type Config struct {
	Network        string `json:"network" yaml:"network"`
	ProxyURL       string `json:"proxy_url,omitempty" yaml:"proxy-url"`
	IwanBinary     string `json:"iwan_binary,omitempty" yaml:"iwan-binary"`
	IwanServer     string `json:"iwan_server,omitempty" yaml:"iwan-server"`
	StateDir       string `json:"state_dir,omitempty" yaml:"state-dir"`
	APIBase        string `json:"api_base,omitempty" yaml:"api-base"`
	BudgetCooldown string `json:"budget_cooldown,omitempty" yaml:"budget-cooldown"`
	DonationMode   string `json:"donation_mode,omitempty" yaml:"donation-mode"`
	DonationOrigin string `json:"donation_origin,omitempty" yaml:"donation-origin"`
}

// DefaultConfig deliberately ignores ambient proxy variables for campus traffic.
func DefaultConfig() Config {
	directory, err := os.UserConfigDir()
	if err != nil {
		directory = "."
	}
	return Config{Network: "auto", IwanBinary: "iwan-client-oidc", IwanServer: "1", StateDir: filepath.Join(directory, "cpa-ustc"), APIBase: APIBase, BudgetCooldown: DefaultBudgetCooldown, DonationMode: "disabled"}
}

// LoadConfig reads the shared CLI/plugin settings without creating directories.
func LoadConfig(path string) (Config, error) {
	config := DefaultConfig()
	if path == "" {
		path = filepath.Join(config.StateDir, "config.json")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if err = json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("invalid settings: %w", err)
	}
	return config, config.Validate()
}

// Validate rejects insecure gateway URLs and ambiguous network settings early.
func (config Config) Validate() error {
	if config.DonationMode != "" && config.DonationMode != "disabled" && config.DonationMode != "open" {
		return errors.New("donation-mode must be disabled or open")
	}
	if config.DonationMode == "open" || config.DonationOrigin != "" {
		origin, err := url.Parse(config.DonationOrigin)
		if err != nil || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Opaque != "" || (origin.Path != "" && origin.Path != "/") || (origin.Scheme != "https" && !(origin.Scheme == "http" && (origin.Hostname() == "localhost" || origin.Hostname() == "127.0.0.1" || origin.Hostname() == "::1"))) {
			return errors.New("donation-origin must be an HTTPS origin without a path; HTTP is allowed only on localhost for testing")
		}
	}
	if delay, err := time.ParseDuration(config.BudgetCooldown); err != nil || delay < time.Second || delay > 7*24*time.Hour {
		return errors.New("budget-cooldown must be a duration between 1s and 168h, such as 30m or 3h")
	}
	if config.Network != "auto" && config.Network != "direct" && config.Network != "iwan" {
		return errors.New("network must be auto, direct or iwan")
	}
	if err := trustedURL(config.APIBase); err != nil {
		return fmt.Errorf("api-base: %w", err)
	}
	if config.ProxyURL != "" {
		proxy, err := url.Parse(config.ProxyURL)
		if err != nil || proxy.Hostname() == "" || proxy.User != nil || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return errors.New("proxy-url must be an HTTP(S) or SOCKS5 URL without credentials")
		}
	}
	if config.StateDir == "" || config.IwanBinary == "" || config.IwanServer == "" {
		return errors.New("state-dir, iwan-binary and iwan-server cannot be empty")
	}
	return nil
}

func trustedURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("a clean HTTPS URL is required")
	}
	return nil
}

// WritePrivate atomically replaces a credential/config file without widening permissions.
func WritePrivate(path string, data []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".cpa-ustc-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func apiBase(raw string) string {
	base := strings.TrimRight(raw, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base
}
