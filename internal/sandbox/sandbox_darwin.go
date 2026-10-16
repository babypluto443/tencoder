// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"strings"
)

// sandboxExecPath 硬编码路径，防止 PATH 注入攻击
const sandboxExecPath = "/usr/bin/sandbox-exec"

// darwinSandbox 基于 macOS seatbelt（sandbox-exec）实现沙箱隔离。
// 通过动态生成 seatbelt profile 控制文件写入和网络访问权限。
type darwinSandbox struct{}

func newPlatformSandbox() Sandbox {
	return &darwinSandbox{}
}

func (s *darwinSandbox) Available() bool {
	_, err := os.Stat(sandboxExecPath)
	return err == nil
}

// buildProfile 动态生成 seatbelt profile 字符串。
// 策略：默认拒绝 → 放行执行/读取 → 按路径放行写入 → 按路径拒绝写入 → 网络控制。
func buildProfile(config Config) string {
	var sb strings.Builder

	sb.WriteString("(version 1)\n")
	sb.WriteString("(deny default)\n")

	// 允许进程执行和 fork
	sb.WriteString("(allow process-exec)\n")
	sb.WriteString("(allow process-fork)\n")
	// 允许读取系统信息
	sb.WriteString("(allow sysctl-read)\n")
	// 全盘可读
	sb.WriteString("(allow file-read* (subpath \"/\"))\n")

	// 按路径放行写入
	for _, path := range config.AllowWrite {
		sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n", path))
	}

	// 拒绝写入的路径放在 allow 之后，seatbelt 后出现的规则优先。
	// 单文件用 literal 精确匹配，目录用 subpath 前缀匹配。
	for _, path := range config.DenyWrite {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			sb.WriteString(fmt.Sprintf("(deny file-write* (subpath %q))\n", path))
		} else {
			sb.WriteString(fmt.Sprintf("(deny file-write* (literal %q))\n", path))
		}
	}

	// 网络控制
	if config.NetworkEnabled {
		sb.WriteString("(allow network*)\n")
	} else {
		sb.WriteString("(deny network*)\n")
	}

	return sb.String()
}

func (s *darwinSandbox) Wrap(command string, config Config) (string, error) {
	profile := buildProfile(config)
	// 用 -p 参数传入 profile 内容，用单引号包裹命令避免 shell 二次解析
	wrapped := fmt.Sprintf("%s -p '%s' bash -c %q", sandboxExecPath, profile, command)
	return wrapped, nil
}
