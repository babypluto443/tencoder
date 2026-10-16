// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

//go:build linux

package sandbox

import (
	"fmt"
	"os/exec"
	"strings"
)

// linuxSandbox 基于 bubblewrap (bwrap) 实现沙箱隔离。
// bwrap 利用 Linux user namespace 创建轻量级隔离环境。
type linuxSandbox struct{}

func newPlatformSandbox() Sandbox {
	return &linuxSandbox{}
}

func (s *linuxSandbox) Available() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}

func (s *linuxSandbox) Wrap(command string, config Config) (string, error) {
	var args []string

	// 隔离 user 和 pid namespace
	args = append(args, "bwrap", "--unshare-user", "--unshare-pid")

	// 根文件系统只读挂载
	args = append(args, "--ro-bind", "/", "/")

	// 按路径放行写入（可写绑定）
	for _, path := range config.AllowWrite {
		args = append(args, "--bind", path, path)
	}

	// 强制只读（覆盖上面可写根路径下的子路径）
	for _, path := range config.DenyWrite {
		args = append(args, "--ro-bind", path, path)
	}

	// 网络隔离
	if !config.NetworkEnabled {
		args = append(args, "--unshare-net")
	}

	// 挂载 /proc，很多命令依赖它
	args = append(args, "--proc", "/proc")

	// 追加要执行的命令
	args = append(args, "--", "bash", "-c", command)

	// 拼接成完整命令字符串，shell 特殊字符需要正确转义
	var sb strings.Builder
	for i, arg := range args {
		if i > 0 {
			sb.WriteByte(' ')
		}
		// 包含空格或特殊字符的参数用单引号包裹
		if strings.ContainsAny(arg, " \t\n\"'\\$`!") {
			sb.WriteString(fmt.Sprintf("'%s'", strings.ReplaceAll(arg, "'", "'\\''")))
		} else {
			sb.WriteString(arg)
		}
	}
	return sb.String(), nil
}
