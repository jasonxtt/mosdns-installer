package coremain

import (
	"embed"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

//go:embed embed_config/*
var embedConfigFS embed.FS

const (
	socks5Placeholder     = "__SOCKS5_PLACEHOLDER__"
	ecsPlaceholder        = "__ECS_PLACEHOLDER__"
	singboxDnsPlaceholder = "__SINGBOX_DNS__"
	dnsPortPlaceholder    = "__DNS_PORT__"
	defaultECS            = "211.136.112.50"
	defaultSingboxDNS     = "127.0.0.1:6666"
	defaultSocks5         = "127.0.0.1:7890"
	defaultDNSPort        = "53"
	webUIPort             = 9099
)

func init() {
	rootCmd.AddCommand(newInstallCmd())
	rootCmd.AddCommand(newReinstallCmd())
	rootCmd.AddCommand(newUninstallCmd())
}

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install mosdns to system with embedded configuration",
		RunE:  runInstall,
	}
}

func newReinstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reinstall",
		Short: "Reinstall mosdns (restore factory settings)",
		RunE:  runReinstall,
	}
}

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Completely uninstall mosdns from system",
		RunE:  runUninstall,
	}
}

func runInstall(cmd *cobra.Command, args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this command must be run as root (use sudo)")
	}

	fmt.Println("[信息] 正在检测Web UI端口...")
	if err := checkWebUIPort(); err != nil {
		fmt.Printf("\n✗ %v\n", err)
		fmt.Println("  请先关闭占用9099端口的程序，或修改mosdns的Web UI端口后重试。")
		return err
	}

	dnsPort := promptDNSPort()
	singboxDNS := promptSingboxDNS()
	socks5 := promptSocks5()

	shouldRelease53 := dnsPort == defaultDNSPort
	if shouldRelease53 {
		fmt.Println("\n[信息] 正在尝试释放53端口...")
		if err := release53Port(); err != nil {
			fmt.Printf("\n✗ 释放53端口失败: %v\n", err)
			fmt.Println("  请先关闭占用53端口的程序后再运行安装。")
			return err
		}
	}

	ecsIP := getECSIP()

	fmt.Println("[1/4] 正在创建目标目录 /cus/mosdns...")
	if err := os.MkdirAll("/cus/mosdns", 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	fmt.Println("[2/4] 正在释放配置文件...")
	if err := extractEmbeddedConfig("/cus/mosdns", socks5, ecsIP, singboxDNS, dnsPort); err != nil {
		return fmt.Errorf("failed to extract config: %w", err)
	}

	fmt.Println("[3/4] 正在安装二进制文件到 /usr/local/bin/mosdns...")
	if err := installSelfBinary(); err != nil {
		return fmt.Errorf("failed to install binary: %w", err)
	}

	fmt.Println("[4/4] 正在安装服务文件...")
	if err := installService(); err != nil {
		return fmt.Errorf("failed to install service: %w", err)
	}

	if err := startService(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	fmt.Println("\n[信息] 安装完成！")
	fmt.Println("\n✓ mosdns 安装成功！")
	fmt.Println("  Web UI: http://<您的IP>:9099")
	fmt.Println("  配置文件: /cus/mosdns/config_custom.yaml")

	if ecsIP == defaultECS {
		fmt.Println("\n⚠ 警告: 无法自动检测到您的公网IP。")
		fmt.Println("  建议安装完毕后通过Web UI手动设置ECS IP。")
	}

	return nil
}

func promptSocks5() string {
	for {
		fmt.Printf("请输入SOCKS5代理（默认 %s，直接回车使用默认值）: ", defaultSocks5)
		var input string
		fmt.Scanln(&input)
		input = strings.TrimSpace(input)

		if input == "" {
			return defaultSocks5
		}
		if isValidIPPort(input) {
			return input
		}
		fmt.Println("✗ 格式错误！请按 IP:端口 格式输入（例如：10.0.0.2:7890）")
	}
}

func promptSingboxDNS() string {
	for {
		fmt.Printf("请输入sing-box DNS端口用于获取国外域名FakeIP（默认 %s，直接回车使用默认值）: ", defaultSingboxDNS)
		var input string
		fmt.Scanln(&input)
		input = strings.TrimSpace(input)

		if input == "" {
			return defaultSingboxDNS
		}
		if isValidIPPort(input) {
			return input
		}
		fmt.Println("✗ 格式错误！请按 IP:端口 格式输入（例如：127.0.0.1:6666）")
	}
}

func promptDNSPort() string {
	for {
		fmt.Printf("请输入DNS监听端口（默认 %s，直接回车使用默认值）: ", defaultDNSPort)
		var input string
		fmt.Scanln(&input)
		input = strings.TrimSpace(input)

		if input == "" {
			input = defaultDNSPort
		}

		portNum := 0
		fmt.Sscanf(input, "%d", &portNum)
		if portNum <= 0 || portNum > 65535 {
			fmt.Println("✗ 端口号无效，请输入1-65535之间的数字")
			continue
		}

		if input == defaultDNSPort {
			return input
		}

		if isPortInUse(input) {
			fmt.Printf("✗ 端口 %s 已被占用，请使用其他端口\n", input)
			continue
		}

		return input
	}
}

func isPortInUse(port string) bool {
	for _, proto := range []string{"tcp", "udp"} {
		ln, err := net.Listen(proto, ":"+port)
		if err != nil {
			if strings.Contains(err.Error(), "permission denied") ||
			   strings.Contains(err.Error(), "操作不被允许") {
				continue
			}
			return true
		}
		ln.Close()
	}
	return false
}

func checkPortWithSystem(port string) bool {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("ss -tuln | grep ':%s '", port))
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func checkWebUIPort() error {
	addr := fmt.Sprintf(":%d", webUIPort)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("Web UI端口 %d 已被占用", webUIPort)
	}
	l.Close()
	return nil
}

func release53Port() error {
	if !checkPortWithSystem(defaultDNSPort) {
		return nil
	}

	initSys := detectInitSystem()
	if initSys != "systemd" {
		return fmt.Errorf("仅支持systemd系统释放53端口，当前系统: %s", initSys)
	}

	confPath := "/etc/systemd/resolved.conf"
	data, err := os.ReadFile(confPath)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	content := string(data)
	hasStubListener := false
	lines := strings.Split(content, "\n")
	newLines := make([]string, 0)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "DNSStubListener=") {
			hasStubListener = true
			if strings.Contains(trimmed, "DNSStubListener=no") {
				newLines = append(newLines, line)
				continue
			}
			newLines = append(newLines, "DNSStubListener=no")
		} else {
			newLines = append(newLines, line)
		}
	}

	if !hasStubListener {
		newLines = append(newLines, "DNSStubListener=no")
	}

	newContent := strings.Join(newLines, "\n")
	if err := os.WriteFile(confPath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	if err := exec.Command("systemctl", "reload-or-restart", "systemd-resolved").Run(); err != nil {
		return fmt.Errorf("重启systemd-resolved失败: %w", err)
	}

	time.Sleep(2 * time.Second)

	if checkPortWithSystem(defaultDNSPort) {
		return fmt.Errorf("53端口仍被占用，请手动检查")
	}

	fmt.Println("[信息] 53端口释放成功")
	return nil
}

func detectInitSystem() string {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd"
	}
	if _, err := os.Stat("/run/openrc"); err == nil {
		return "openrc"
	}
	return "unknown"
}

func isValidIPPort(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}

	ip := parts[0]
	port := parts[1]

	if net.ParseIP(ip) == nil {
		return false
	}

	portNum := 0
	fmt.Sscanf(port, "%d", &portNum)
	if portNum <= 0 || portNum > 65535 {
		return false
	}

	return true
}

func getECSIP() string {
	fmt.Println("\n[信息] 正在检测您的公网IP...")

	if err := exec.Command("sh", "-c", "which curl").Run(); err != nil {
		fmt.Println("[信息] 未检测到curl，正在尝试安装...")
		if installErr := exec.Command("sh", "-c", "apt-get update && apt-get install -y curl").Run(); installErr != nil {
			fmt.Printf("[警告] 安装curl失败: %v，将使用默认ECS IP\n", installErr)
			return defaultECS
		}
	}

	cmd := exec.Command("curl", "-s", "4.ipw.cn")
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("[警告] 检测IP失败: %v，将使用默认ECS IP\n", err)
		return defaultECS
	}

	ip := strings.TrimSpace(string(output))
	if net.ParseIP(ip) == nil {
		fmt.Printf("[警告] 检测到的IP无效: %s，将使用默认ECS IP\n", ip)
		return defaultECS
	}

	fmt.Printf("[信息] 检测到IP: %s\n", ip)
	return ip
}

func runReinstall(cmd *cobra.Command, args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("此命令需要root权限（请使用sudo）")
	}

	fmt.Println("警告：这将恢复出厂设置并覆盖 /cus/mosdns 中的所有配置")
	fmt.Print("是否继续？(y/N): ")
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "y" && confirm != "Y" {
		fmt.Println("已取消。")
		return nil
	}

	singboxDNS := promptSingboxDNS()
	socks5 := promptSocks5()
	dnsPort := promptDNSPort()

	shouldRelease53 := dnsPort == defaultDNSPort
	if shouldRelease53 {
		fmt.Println("\n[信息] 正在尝试释放53端口...")
		if err := release53Port(); err != nil {
			fmt.Printf("\n✗ 释放53端口失败: %v\n", err)
			fmt.Println("  请先关闭占用53端口的程序后再运行安装。")
			return err
		}
	}

	ecsIP := getECSIP()

	fmt.Println("\n[1/4] 正在停止 mosdns 服务...")
	stopService()

	fmt.Println("[2/4] 正在重新释放配置文件...")
	if err := extractEmbeddedConfig("/cus/mosdns", socks5, ecsIP, singboxDNS, dnsPort); err != nil {
		return fmt.Errorf("failed to extract config: %w", err)
	}

	fmt.Println("[3/4] 正在安装服务文件...")
	if err := installService(); err != nil {
		fmt.Printf("警告: 安装服务失败: %v\n", err)
	}

	fmt.Println("[4/4] 正在重启 mosdns 服务...")
	startService()

	fmt.Println("\n✓ mosdns 重新安装成功！")
	return nil
}

func runUninstall(cmd *cobra.Command, args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("此命令需要root权限（请使用sudo）")
	}

	fmt.Println("警告：这将完全从系统中移除 mosdns")
	fmt.Print("是否继续？(y/N): ")
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "y" && confirm != "Y" {
		fmt.Println("已取消。")
		return nil
	}

	fmt.Println("\n[1/4] 正在停止 mosdns 服务...")
	stopService()

	fmt.Println("[2/4] 正在删除服务文件...")
	removeService()

	fmt.Println("[3/4] 正在删除二进制文件...")
	if err := os.Remove("/usr/local/bin/mosdns"); err != nil {
		fmt.Printf("警告: 删除二进制文件失败: %v\n", err)
	}

	fmt.Println("[4/4] 正在删除配置目录...")
	fmt.Print("是否删除 /cus/mosdns 目录？(y/N): ")
	var confirmConfig string
	fmt.Scanln(&confirmConfig)
	if confirmConfig == "y" || confirmConfig == "Y" {
		if err := os.RemoveAll("/cus/mosdns"); err != nil {
			fmt.Printf("警告: 删除配置目录失败: %v\n", err)
		}
	}

	fmt.Println("\n✓ mosdns 卸载成功！")
	return nil
}

func extractEmbeddedConfig(targetDir, socks5, ecsIP, singboxDNS, dnsPort string) error {
	return fs.WalkDir(embedConfigFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == "." {
			return nil
		}

		relPath := strings.TrimPrefix(path, "embed_config/")
		targetPath := filepath.Join(targetDir, relPath)
		targetDirPath := filepath.Dir(targetPath)

		if err := os.MkdirAll(targetDirPath, 0755); err != nil {
			return err
		}

		data, err := embedConfigFS.ReadFile(path)
		if err != nil {
			return err
		}

		content := string(data)
		content = strings.ReplaceAll(content, socks5Placeholder, socks5)
		content = strings.ReplaceAll(content, ecsPlaceholder, ecsIP)
		content = strings.ReplaceAll(content, singboxDnsPlaceholder, singboxDNS)
		content = strings.ReplaceAll(content, dnsPortPlaceholder, dnsPort)

		if err := os.WriteFile(targetPath, []byte(content), 0644); err != nil {
			return err
		}

		return nil
	})
}

func installSelfBinary() error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	data, err := os.ReadFile(selfPath)
	if err != nil {
		return fmt.Errorf("failed to read self: %w", err)
	}

	if err := os.WriteFile("/usr/local/bin/mosdns", data, 0755); err != nil {
		return fmt.Errorf("failed to write binary: %w", err)
	}

	return nil
}

func installService() error {
	if isSystemd() {
		return installSystemdService()
	} else if isOpenRC() {
		return installOpenRCService()
	}
	return fmt.Errorf("unsupported init system")
}

func installSystemdService() error {
	serviceContent := `[Unit]
Description=A DNS forwarder
ConditionFileIsExecutable=/usr/local/bin/mosdns

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart=/usr/local/bin/mosdns "start" "--as-service" "-d" "/cus/mosdns" "-c" "/cus/mosdns/config_custom.yaml"

Restart=always
RestartSec=120
EnvironmentFile=-/etc/sysconfig/mosdns

[Install]
WantedBy=multi-user.target
`

	if err := os.WriteFile("/etc/systemd/system/mosdns.service", []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	return nil
}

func installOpenRCService() error {
	serviceContent := `#!/sbin/openrc-run

name="mosdns"
description="A DNS forwarder"
command="/usr/local/bin/mosdns"
command_args="start --as-service -d /cus/mosdns -c /cus/mosdns/config_custom.yaml"
pidfile="/run/${RC_SVCNAME}.pid"
command_background="yes"

depend() {
    need net
    after firewall
}
`

	if err := os.WriteFile("/etc/init.d/mosdns", []byte(serviceContent), 0755); err != nil {
		return fmt.Errorf("failed to write init script: %w", err)
	}

	return nil
}

func startService() error {
	if isSystemd() {
		if err := exec.Command("systemctl", "enable", "--now", "mosdns").Run(); err != nil {
			return fmt.Errorf("failed to start service: %w", err)
		}
	} else if isOpenRC() {
		if err := exec.Command("rc-service", "mosdns", "start").Run(); err != nil {
			return fmt.Errorf("failed to start service: %w", err)
		}
		if err := exec.Command("rc-update", "add", "mosdns", "default").Run(); err != nil {
			return fmt.Errorf("failed to enable service: %w", err)
		}
	}
	return nil
}

func stopService() {
	if isSystemd() {
		exec.Command("systemctl", "stop", "mosdns").Run()
		exec.Command("systemctl", "disable", "mosdns").Run()
	} else if isOpenRC() {
		exec.Command("rc-service", "mosdns", "stop").Run()
		exec.Command("rc-update", "del", "mosdns", "default").Run()
	}
}

func removeService() {
	if isSystemd() {
		os.Remove("/etc/systemd/system/mosdns.service")
		exec.Command("systemctl", "daemon-reload").Run()
	} else if isOpenRC() {
		os.Remove("/etc/init.d/mosdns")
	}
}

func isSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return true
	}
	if _, err := os.Stat("/usr/lib/systemd/system"); err == nil {
		return true
	}
	return false
}

func isOpenRC() bool {
	if _, err := os.Stat("/run/openrc"); err == nil {
		return true
	}
	if _, err := exec.LookPath("rc-service"); err == nil {
		return true
	}
	return false
}

var _ = regexp.MustCompile("")
