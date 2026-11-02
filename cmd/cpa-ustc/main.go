// cpa-ustc configures CPA and signs in without collecting a university password.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cpa-ustc/internal/setup"
	"cpa-ustc/internal/ustc"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		usage()
		return nil
	}
	command := arguments[0]
	if command == "help" || command == "--help" || command == "-h" {
		usage()
		return nil
	}
	if command == "version" || command == "--version" {
		fmt.Println("cpa-ustc", ustc.Version)
		return nil
	}
	if command != "setup" && command != "install" && command != "login" && command != "models" && command != "doctor" && command != "install-iwan" && command != "iwan-login" {
		return errors.New("未知命令；运行 cpa-ustc help 查看帮助")
	}
	config, err := ustc.LoadConfig("")
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("cpa-config", "config.yaml", "CLIProxyAPI 配置路径")
	authOverride := flags.String("auth-dir", "", "覆盖 CPA 凭据目录（默认沿用已有配置）")
	pluginDirectory := flags.String("plugin-dir", "", "覆盖 CPA 插件目录（默认沿用已有配置）")
	executable, _ := os.Executable()
	pluginSource := flags.String("plugin-source", filepath.Join(filepath.Dir(executable), setup.LibraryName()), "已构建的动态库路径")
	flags.StringVar(&config.Network, "network", config.Network, "auto / direct / iwan")
	flags.StringVar(&config.ProxyURL, "proxy-url", config.ProxyURL, "已有 HTTP(S)/SOCKS5 代理 URL")
	flags.StringVar(&config.IwanBinary, "iwan-bin", config.IwanBinary, "iwan-client-oidc 路径")
	flags.StringVar(&config.IwanServer, "iwan-server", config.IwanServer, "iWAN 线路名称或序号")
	flags.StringVar(&config.StateDir, "state-dir", config.StateDir, "公开配置与 iWAN 会话目录")
	flags.StringVar(&config.APIBase, "api-base", config.APIBase, "直接 API Key 模式的 HTTPS 基础地址")
	flags.StringVar(&config.BudgetCooldown, "budget-cooldown", config.BudgetCooldown, "预算耗尽且无重试提示时的凭据冷却时长，如 30m、3h")
	flags.StringVar(&config.DonationMode, "donation-mode", config.DonationMode, "公开捐献入口：disabled / open（默认关闭）")
	flags.StringVar(&config.DonationOrigin, "donation-origin", config.DonationOrigin, "捐献页面的 HTTPS 站点源，不含路径")
	keyEnvironment := flags.String("api-key-env", "", "直接使用此环境变量中的 API Key，不将值写入磁盘")
	keyStdin := flags.Bool("api-key-stdin", false, "从标准输入读取 API Key，保存到私有凭据文件")
	installIwan := flags.Bool("install-iwan", false, "下载并校验固定版本的第三方 iWAN 命令行客户端")
	noBrowser := flags.Bool("no-browser", false, "只输出官方登录链接，不自动打开浏览器")
	callbackPort := flags.Int("callback-port", 0, "浏览器本机回调端口（默认随机；远程主机需 SSH 转发）")
	accountPath := flags.String("account", "", "models 命令使用的特定凭据文件")
	if err = flags.Parse(arguments[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("不接受额外位置参数")
	}
	if *keyEnvironment != "" && *keyStdin {
		return errors.New("--api-key-env 与 --api-key-stdin 不能同时使用")
	}
	config.StateDir, err = setup.ExpandPath(config.StateDir)
	if err != nil {
		return err
	}
	if err = config.Validate(); err != nil {
		return err
	}
	if command == "setup" || command == "install" {
		info, statErr := os.Stat(*pluginSource)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return errors.New("找不到有效的原生动态库；请先 make build，或指定 --plugin-source")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if command == "install-iwan" || *installIwan {
		fmt.Fprintln(os.Stderr, "安装第三方 ustc-iwan", setup.IwanRelease, "（验证 GitHub SHA-256，不需要 root）")
		config.IwanBinary, err = setup.InstallIwan(ctx, config.StateDir)
		if err != nil {
			return err
		}
		if command == "install-iwan" {
			return saveSettings(config)
		}
	}
	service := ustc.NewService(config)
	defer service.Close()
	if command == "iwan-login" {
		if err = service.Network.Login(ctx, os.Stdin, os.Stderr); err != nil {
			return err
		}
		return saveSettings(config)
	}
	if command == "doctor" {
		campus, probeErr := ustc.OnCampus(ctx)
		if probeErr != nil {
			fmt.Println("校园网检测:", probeErr)
		} else {
			fmt.Println("校园网:", campus)
		}
		binary, lookErr := exec.LookPath(config.IwanBinary)
		if lookErr != nil {
			fmt.Println("iWAN 客户端: 未安装（已连接官方客户端时可忽略）")
		} else {
			fmt.Println("iWAN 客户端:", binary)
		}
		fmt.Println("网络模式:", config.Network, "；状态目录:", config.StateDir)
		return probeErr
	}
	path, err := setup.ExpandPath(*configPath)
	if err != nil {
		return err
	}
	authDir, err := setup.AuthDir(path, *authOverride)
	if err != nil {
		return err
	}
	if command == "install" {
		if *keyEnvironment != "" || *keyStdin {
			return errors.New("install 不接收凭据，请在网页中添加账号，或使用 setup/login")
		}
		if err = os.MkdirAll(authDir, 0700); err != nil {
			return err
		}
		return installPlugin(path, *pluginSource, *pluginDirectory, authDir, config)
	}
	if command == "models" {
		file := *accountPath
		if file == "" {
			matches, globErr := filepath.Glob(filepath.Join(authDir, "ustc-*.json"))
			if globErr != nil {
				return globErr
			}
			if len(matches) != 1 {
				return errors.New("请用 --account 指定凭据文件（当前目录为零个或多个科大账号）")
			}
			file = matches[0]
		}
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			return readErr
		}
		var account ustc.Account
		if json.Unmarshal(data, &account) != nil {
			return errors.New("凭据文件格式无效")
		}
		models, updated, modelErr := service.Models(ctx, account)
		if updated.AccessToken != account.AccessToken || updated.RefreshToken != account.RefreshToken || !updated.ExpiresAt.Equal(account.ExpiresAt) {
			data, _ = json.MarshalIndent(updated, "", "  ")
			if err = ustc.WritePrivate(file, data); err != nil {
				return err
			}
		}
		if modelErr != nil {
			return modelErr
		}
		for _, model := range models {
			fmt.Println(model.ID)
		}
		return nil
	}
	var account ustc.Account
	if *keyEnvironment != "" || *keyStdin {
		key := ""
		if *keyStdin {
			data, readErr := io.ReadAll(io.LimitReader(os.Stdin, 65537))
			if readErr != nil {
				return readErr
			}
			if len(data) > 65536 {
				return errors.New("API Key 输入过长")
			}
			key = strings.TrimSpace(string(data))
		} else if os.Getenv(*keyEnvironment) == "" {
			return errors.New("指定的 API Key 环境变量为空")
		}
		account, err = ustc.APIAccount(key, *keyEnvironment, config)
		if err != nil {
			return err
		}
	} else {
		login, loginErr := service.StartLogin(ctx, *callbackPort)
		if loginErr != nil {
			return loginErr
		}
		defer login.Close()
		fmt.Fprintln(os.Stderr, "请仅在 id.ustc.edu.cn 官方页面完成登录（支持通行密钥/多因素认证）：\n"+login.URL)
		if !*noBrowser {
			if err = openBrowser(ctx, login.URL); err != nil {
				fmt.Fprintln(os.Stderr, "无法自动打开浏览器，请手动打开上述链接。")
			}
		}
		account, err = login.Wait(ctx)
		if err != nil {
			return err
		}
		// Validate entitlement before reporting a successful one-step setup.
		_, account, err = service.Resolve(ctx, account)
		if err != nil {
			return err
		}
	}
	input := os.Stdin
	if *keyStdin {
		terminalName := "/dev/tty"
		if runtime.GOOS == "windows" {
			terminalName = "CONIN$"
		}
		if terminal, openErr := os.Open(terminalName); openErr == nil {
			defer terminal.Close()
			input = terminal
		}
	}
	if err = service.Network.PrepareLogin(ctx, input, os.Stderr); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(account, "", "  ")
	file := filepath.Join(authDir, account.ID+".json")
	if err = ustc.WritePrivate(file, data); err != nil {
		return err
	}
	if err = saveSettings(config); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "凭据已安全保存:", file)
	if command == "login" {
		return nil
	}
	return installPlugin(path, *pluginSource, *pluginDirectory, authDir, config)
}

// installPlugin bootstraps CPA without requiring a school login or network probe.
// Users may add every credential later through the plugin's browser resource.
func installPlugin(path, source, directory, authDir string, config ustc.Config) error {
	pluginDir, err := setup.PluginDir(path, directory)
	if err != nil {
		return err
	}
	if err = setup.InstallLibrary(source, pluginDir); err != nil {
		return err
	}
	if err = setup.Configure(path, pluginDir, authDir, config); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "原生插件与 CPA 配置已就绪:", path, "\n启动或重启 CLIProxyAPI 后即可使用；现有设置保留，原配置已备份。")
	fmt.Fprintln(os.Stderr, "网页设置：打开 CPA 管理中心的插件菜单 → 中科大词元计划；账号加入 CPA 自身号池。")
	return nil
}

func saveSettings(config ustc.Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return ustc.WritePrivate(filepath.Join(config.StateDir, "config.json"), data)
}

func openBrowser(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	// Some desktop launchers stay alive with the browser; never block the callback
	// or kill the user's browser when the CLI's login context finishes.
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func usage() {
	_, _ = io.WriteString(os.Stdout, `cpa-ustc — CLIProxyAPI 中科大词元计划原生插件

命令：
  install       仅安装动态库并启用插件；随后在 CPA 网页中设置和登录
  setup         浏览器科大登录 + 安装动态库 + 合并 CPA 配置
  login         仅登录并写入 CPA 凭据目录
  models        获取当前账号可用模型（多个账号时指定 --account）
  doctor        不携带凭据地检查校园网络和 iWAN 安装
  install-iwan  显式安装经过摘要校验的第三方 iWAN 命令行客户端
  iwan-login    登录或更新过期的 iWAN 线路凭据
  version       显示插件版本

示例：
  cpa-ustc setup --cpa-config ./config.yaml
  cpa-ustc setup --install-iwan --cpa-config ./config.yaml
  cpa-ustc setup --api-key-env USTC_API_KEY
  printf '%s' "$USTC_API_KEY" | cpa-ustc setup --api-key-stdin
  cpa-ustc setup --network direct

运行 cpa-ustc <命令> --help 查看全部选项。
`)
}
