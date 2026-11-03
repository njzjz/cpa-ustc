package ustc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Network owns only the iWAN child it starts; existing system tunnels are untouched.
type Network struct {
	config  Config
	mu      sync.Mutex
	process *exec.Cmd
	exited  chan struct{}
	proxy   string
	checked time.Time
	direct  bool
	probe   func(context.Context) (bool, error)
}

func NewNetwork(config Config) *Network { return &Network{config: config} }

// HTTPClient disables environment proxies and refuses redirects carrying credentials.
func HTTPClient(proxy string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 5 * time.Minute
	transport.TLSHandshakeTimeout = 15 * time.Second
	if proxy != "" {
		parsed, _ := url.Parse(proxy)
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

// OnCampus uses the school's non-credentialed probe, never an inference request.
func OnCampus(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, CampusCheckURL, nil)
	client := HTTPClient("")
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("校园网检测失败，请检查网络或明确指定 --network direct/iwan")
	}
	defer response.Body.Close()
	var result struct {
		Campus json.RawMessage `json:"campus"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&result) != nil {
		return false, errors.New("校园网检测返回无效结果")
	}
	return parseCampus(result.Campus)
}

func parseCampus(raw json.RawMessage) (bool, error) {
	switch string(raw) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, errors.New("校园网检测返回无效结果")
	}
}

// Proxy selects the campus path before any credential-bearing inference request.
func (network *Network) Proxy(ctx context.Context) (string, error) {
	network.mu.Lock()
	defer network.mu.Unlock()
	if network.config.ProxyURL != "" {
		return network.config.ProxyURL, nil
	}
	if network.config.Network == "direct" {
		return "", nil
	}
	if network.config.Network == "auto" {
		if time.Since(network.checked) > 30*time.Second {
			probe := network.probe
			if probe == nil {
				probe = OnCampus
			}
			direct, err := probe(ctx)
			if err != nil {
				return "", err
			}
			network.direct = direct
			network.checked = time.Now()
		}
		if network.direct {
			return "", nil
		}
	}
	if network.process != nil {
		select {
		case <-network.exited:
			network.process = nil
			network.proxy = ""
		default:
			return network.proxy, nil
		}
	}
	configDir := filepath.Join(network.config.StateDir, "iwan")
	if _, err := os.Stat(filepath.Join(configDir, "servers.json")); err != nil {
		return "", errors.New("尚未登录 iWAN；请先运行 cpa-ustc setup（或连接官方 iWAN 客户端后重试）")
	}
	if err := securePrivateDirectory(configDir); err != nil {
		return "", err
	}
	if err := securePrivatePath(filepath.Join(configDir, "servers.json")); err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	listener.Close()
	command := exec.Command(network.config.IwanBinary, "--connect", "--server", network.config.IwanServer, "--http", "--http-listen", address, "--config-dir", configDir)
	// No shell, passwords, debug logs or inherited stdin enter the background process.
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err = command.Start(); err != nil {
		return "", fmt.Errorf("无法启动 iWAN 客户端，请安装 iwan-client-oidc 或设置 iwan-binary: %w", err)
	}
	network.process = command
	network.exited = make(chan struct{})
	exited := network.exited
	go func() { _ = command.Wait(); close(exited) }()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			network.stopLocked()
			return "", ctx.Err()
		case <-deadline.C:
			network.stopLocked()
			return "", errors.New("iWAN 代理启动超时，请重新登录 iWAN 或检查所选线路")
		case <-exited:
			network.process = nil
			return "", errors.New("iWAN 客户端退出，请重新运行 cpa-ustc setup 或检查线路")
		case <-ticker.C:
			connection, dialErr := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if dialErr == nil {
				connection.Close()
				network.proxy = "http://" + address
				return network.proxy, nil
			}
		}
	}
}

// PrepareLogin runs the upstream iWAN SSO helper interactively only when needed.
func (network *Network) PrepareLogin(ctx context.Context, input io.Reader, output io.Writer) error {
	if network.config.ProxyURL != "" || network.config.Network == "direct" {
		return nil
	}
	if network.config.Network == "auto" {
		campus, err := OnCampus(ctx)
		if err != nil {
			return err
		}
		if campus {
			return nil
		}
	}
	directory := filepath.Join(network.config.StateDir, "iwan")
	if _, err := os.Stat(filepath.Join(directory, "servers.json")); errors.Is(err, os.ErrNotExist) {
		if err = network.Login(ctx, input, output); err != nil {
			return err
		}
	}
	_, err := network.Proxy(ctx)
	return err
}

// Login explicitly refreshes iWAN's SSO-derived line credentials, including stale ones.
func (network *Network) Login(ctx context.Context, input io.Reader, output io.Writer) error {
	network.Close()
	directory := filepath.Join(network.config.StateDir, "iwan")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	// Set inheritable Windows ACLs before the helper creates any credentials.
	if err := securePrivateDirectory(directory); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, network.config.IwanBinary, "--fetch", "--config-dir", directory)
	command.Stdin = input
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		return fmt.Errorf("iWAN 登录失败；请安装 iwan-client-oidc，或先连接官方客户端；过期时运行 cpa-ustc iwan-login: %w", err)
	}
	return securePrivatePath(filepath.Join(directory, "servers.json"))
}

func (network *Network) stopLocked() {
	if network.process != nil {
		_ = network.process.Process.Kill()
		<-network.exited
		network.process = nil
		network.proxy = ""
	}
}

// Close terminates and reaps only this instance's managed iWAN child.
func (network *Network) Close() { network.mu.Lock(); defer network.mu.Unlock(); network.stopLocked() }
