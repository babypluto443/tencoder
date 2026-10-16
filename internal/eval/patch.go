package eval

import (
	"strings"
)

// TestFilePaths 从 test_patch（unified diff）中提取所有被改动的文件路径。
// 规则：取 "+++ b/<path>" 行（忽略 /dev/null），去掉 b/ 前缀。
func TestFilePaths(testPatch string) map[string]bool {
	files := map[string]bool{}
	for _, line := range strings.Split(testPatch, "\n") {
		if !strings.HasPrefix(line, "+++ ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
		if p == "/dev/null" {
			continue
		}
		p = strings.TrimPrefix(p, "b/")
		if p != "" {
			files[p] = true
		}
	}
	return files
}

// StripTestFiles 从 model_patch 中剔除与 test 文件相关的 diff 段落。
// 对应 SWE-bench 协议："模型不得修改测试文件"——即使模型动了测试文件，
// harness 也不把这部分带进评测（test_patch 会覆盖测试文件）。
// 切分单位是 "diff --git " 行。
func StripTestFiles(modelPatch string, testFiles map[string]bool) string {
	if len(testFiles) == 0 || modelPatch == "" {
		return modelPatch
	}
	lines := strings.Split(modelPatch, "\n")
	var sections [][]string
	cur := []string{}
	for _, ln := range lines {
		if strings.HasPrefix(ln, "diff --git ") && len(cur) > 0 {
			sections = append(sections, cur)
			cur = []string{}
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		sections = append(sections, cur)
	}

	var kept []string
	for _, sec := range sections {
		if sectionTargetsTestFile(sec, testFiles) {
			continue
		}
		kept = append(kept, strings.Join(sec, "\n"))
	}
	out := strings.Join(kept, "\n")
	if strings.TrimSpace(out) == "" {
		return ""
	}
	// 保证末尾换行，git apply 更友好
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// sectionTargetsTestFile 判断一个 diff 段落是否触碰测试文件。
// 同时检查 +++ 行和 --- 行（重命名/删除场景 +++ 可能是 /dev/null）。
func sectionTargetsTestFile(section []string, testFiles map[string]bool) bool {
	for _, ln := range section {
		if strings.HasPrefix(ln, "+++ ") {
			p := strings.TrimPrefix(strings.TrimSpace(ln[4:]), "b/")
			if testFiles[p] {
				return true
			}
		}
		if strings.HasPrefix(ln, "--- ") {
			p := strings.TrimPrefix(strings.TrimSpace(ln[4:]), "a/")
			if testFiles[p] {
				return true
			}
		}
	}
	return false
}
