// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package commands

// SandboxMode 定义沙箱的三种运行模式
type SandboxMode int

const (
	SandboxAutoAllow SandboxMode = iota // 沙箱 + 自动放行（推荐）
	SandboxRegular                       // 沙箱 + 常规权限确认
	SandboxOff                           // 关闭沙箱
)

// SandboxModeLabels 返回三种模式的显示标签
func SandboxModeLabels() []string {
	return []string{
		"开启沙箱 + 自动放行（推荐）",
		"开启沙箱 + 常规权限",
		"关闭沙箱",
	}
}

// SandboxModeDescriptions 返回各模式的说明文字
func SandboxModeDescriptions() []string {
	return []string{
		"命令自动在沙箱内执行，无需确认。显式 deny 规则仍生效。",
		"命令在沙箱内执行，但仍需权限确认。",
		"不使用 OS 级隔离，仅依赖应用层权限。",
	}
}
