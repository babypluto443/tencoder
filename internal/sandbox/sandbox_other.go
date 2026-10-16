// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

//go:build !darwin && !linux

package sandbox

// 不支持沙箱的平台返回 nil，调用方需判空
func newPlatformSandbox() Sandbox {
	return nil
}
