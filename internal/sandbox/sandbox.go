// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package sandbox

// Sandbox 定义 OS 级沙箱的统一接口。
// 不同平台（macOS seatbelt / Linux bubblewrap）各自实现。
type Sandbox interface {
	// Wrap 将原始命令包装成沙箱内执行的命令字符串
	Wrap(command string, config Config) (string, error)
	// Available 检查当前平台的沙箱工具是否可用
	Available() bool
}

// Config 控制沙箱的读写和网络权限
type Config struct {
	AllowWrite     []string // 允许写入的路径列表
	DenyWrite      []string // 始终只读的路径（优先级高于 AllowWrite）
	NetworkEnabled bool     // 是否允许网络访问
}

// New 返回当前平台对应的沙箱实现，不支持的平台返回 nil。
func New() Sandbox {
	return newPlatformSandbox()
}
