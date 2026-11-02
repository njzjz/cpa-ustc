// Package setup installs the native library and merges CPA YAML non-destructively.
package setup

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cpa-ustc/internal/ustc"
	"gopkg.in/yaml.v3"
)

// ExpandPath expands only the current user's home, never shell expressions.
func ExpandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	return filepath.Abs(path)
}

func readDocument(path string) (*yaml.Node, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	var document yaml.Node
	if len(data) > 0 {
		if err = yaml.Unmarshal(data, &document); err != nil {
			return nil, nil, fmt.Errorf("CPA 配置 YAML 无效: %w", err)
		}
	}
	if len(document.Content) == 0 {
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("CPA 配置必须是 YAML 对象")
	}
	return &document, data, nil
}

func field(node *yaml.Node, name string) *yaml.Node {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == name {
			return node.Content[index+1]
		}
	}
	return nil
}

func set(node *yaml.Node, name string, value any) {
	var replacement yaml.Node
	_ = replacement.Encode(value)
	if existing := field(node, name); existing != nil {
		replacement.HeadComment = existing.HeadComment
		replacement.LineComment = existing.LineComment
		*existing = replacement
		return
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, &replacement)
}

func mapping(node *yaml.Node, name string) (*yaml.Node, error) {
	child := field(node, name)
	if child == nil {
		set(node, name, map[string]any{})
		child = field(node, name)
	}
	if child.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("CPA 的 %s 必须是 YAML 对象，拒绝覆盖原有值", name)
	}
	return child, nil
}

// AuthDir respects the existing CPA setting, relative to its config directory.
func AuthDir(configPath, override string) (string, error) {
	if override != "" {
		return ExpandPath(override)
	}
	document, _, err := readDocument(configPath)
	if err != nil {
		return "", err
	}
	directory := "~/.cli-proxy-api"
	if value := field(document.Content[0], "auth-dir"); value != nil && value.Value != "" {
		directory = value.Value
		if !filepath.IsAbs(directory) && !strings.HasPrefix(directory, "~") {
			directory = filepath.Join(filepath.Dir(configPath), directory)
		}
	}
	return ExpandPath(directory)
}

// Configure merges only this provider, retaining unrelated settings and a private backup.
func Configure(path, pluginDir, authDir string, config ustc.Config) error {
	document, original, err := readDocument(path)
	if err != nil {
		return err
	}
	root := document.Content[0]
	plugins, err := mapping(root, "plugins")
	if err != nil {
		return err
	}
	if pluginDir == "" {
		if old := field(plugins, "dir"); old != nil {
			pluginDir = old.Value
		}
	}
	if pluginDir == "" {
		pluginDir = filepath.Join(filepath.Dir(path), "plugins")
	}
	if !filepath.IsAbs(pluginDir) {
		pluginDir = filepath.Join(filepath.Dir(path), pluginDir)
	}
	pluginDir, err = filepath.Abs(pluginDir)
	if err != nil {
		return err
	}
	set(root, "auth-dir", authDir)
	if len(original) == 0 {
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			return err
		}
		set(root, "host", "127.0.0.1")
		set(root, "port", 8317)
		set(root, "api-keys", []string{"cpa-" + base64.RawURLEncoding.EncodeToString(secret)})
	}
	set(plugins, "enabled", true)
	set(plugins, "dir", pluginDir)
	configs, err := mapping(plugins, "configs")
	if err != nil {
		return err
	}
	provider, err := mapping(configs, "ustc")
	if err != nil {
		return err
	}
	for name, value := range map[string]any{"enabled": true, "priority": 10, "network": config.Network, "state-dir": config.StateDir, "iwan-binary": config.IwanBinary, "iwan-server": config.IwanServer, "proxy-url": config.ProxyURL, "api-base": config.APIBase, "budget-cooldown": config.BudgetCooldown, "donation-mode": config.DonationMode, "donation-origin": config.DonationOrigin} {
		set(provider, name, value)
	}
	data, err := yaml.Marshal(document)
	if err != nil {
		return err
	}
	if len(original) > 0 {
		backup := path + ".cpa-ustc-" + time.Now().Format("20060102T150405.000000000") + ".bak"
		if err = ustc.WritePrivate(backup, original); err != nil {
			return err
		}
	}
	return ustc.WritePrivate(path, data)
}

// LibraryName matches CPA's filename-derived plugin ID on each supported platform.
func LibraryName() string {
	switch runtime.GOOS {
	case "windows":
		return "ustc.dll"
	case "darwin":
		return "ustc.dylib"
	default:
		return "ustc.so"
	}
}

// InstallLibrary copies a locally built library; it never downloads unverified plugin code.
func InstallLibrary(source, directory string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("找不到原生插件，请先运行 make build，或通过 --plugin-source 指定: %w", err)
	}
	if len(data) == 0 {
		return errors.New("原生插件文件为空")
	}
	return ustc.WritePrivate(filepath.Join(directory, LibraryName()), data)
}

// PluginDir resolves the directory without moving any previously installed plugins.
func PluginDir(configPath, override string) (string, error) {
	if override != "" {
		return ExpandPath(override)
	}
	document, _, err := readDocument(configPath)
	if err != nil {
		return "", err
	}
	directory := "plugins"
	if plugins := field(document.Content[0], "plugins"); plugins != nil {
		if value := field(plugins, "dir"); value != nil && value.Value != "" {
			directory = value.Value
		}
	}
	if !filepath.IsAbs(directory) && !strings.HasPrefix(directory, "~") {
		directory = filepath.Join(filepath.Dir(configPath), directory)
	}
	return ExpandPath(directory)
}
