// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package memory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxIncludeDepth is the maximum nesting depth for @include directives.
const MaxIncludeDepth = 5

// InstructionSource is one loaded instruction file.
type InstructionSource struct {
	Path    string
	Content string
}

// LoadInstructions discovers and concatenates project & user instruction files.
//
// Discovery order (each later layer is appended later, so model attention prioritises it):
//  1. User global: ~/.tencoder/TENCODER.md, ~/.tencoder/AGENTS.md
//  2. Project: walk from git root down to workDir, picking up TENCODER.md
//     and AGENTS.md in each directory (so the file closest to cwd wins)
//  3. workDir/.tencoder/INSTRUCTIONS.md (legacy)
//  4. workDir/TENCODER.local.md (private local override)
//
// @-include directives:
//   - @./relative/path, @~/home/path, or @/absolute/path
//   - Resolved relative to the including file's directory
//   - Skipped inside fenced code blocks
//   - Cycle-safe (same absolute path is never included twice)
func LoadInstructions(workDir string) string {
	sources := DiscoverInstructions(workDir)
	if len(sources) == 0 {
		return ""
	}
	var parts []string
	for _, s := range sources {
		label := s.Path
		if rel, err := filepath.Rel(workDir, s.Path); err == nil && !strings.HasPrefix(rel, "..") {
			label = rel
		}
		parts = append(parts, fmt.Sprintf("Contents of %s:\n\n%s", label, strings.TrimRight(s.Content, "\n")))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// DiscoverInstructions returns the loaded source files in priority order
// (lowest priority first). Used by LoadInstructions and exposed for tests.
func DiscoverInstructions(workDir string) []InstructionSource {
	var sources []InstructionSource
	seen := map[string]bool{}

	// 确定项目根目录，用于 @include 路径边界检查
	absWorkDir, _ := filepath.Abs(workDir)
	projectRoot := findGitRoot(absWorkDir)
	if projectRoot == "" {
		projectRoot = absWorkDir
	}

	if home, err := os.UserHomeDir(); err == nil {
		add(&sources, seen, filepath.Join(home, ".tencoder", "TENCODER.md"), projectRoot)
		add(&sources, seen, filepath.Join(home, ".tencoder", "AGENTS.md"), projectRoot)
	}
	for _, dir := range projectInstructionDirs(workDir) {
		add(&sources, seen, filepath.Join(dir, "TENCODER.md"), projectRoot)
		add(&sources, seen, filepath.Join(dir, "AGENTS.md"), projectRoot)
	}
	add(&sources, seen, filepath.Join(workDir, ".tencoder", "INSTRUCTIONS.md"), projectRoot)
	add(&sources, seen, filepath.Join(workDir, "TENCODER.local.md"), projectRoot)
	return sources
}

func add(out *[]InstructionSource, seen map[string]bool, path, projectRoot string) {
	abs, err := filepath.Abs(path)
	if err != nil || seen[abs] {
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	seen[abs] = true
	content := expandIncludes(string(data), filepath.Dir(abs), projectRoot, seen, 0)
	*out = append(*out, InstructionSource{Path: abs, Content: content})
}

// expandIncludes 展开 @include 指令。projectRoot 用于边界检查，
// 防止通过 ../ 逃逸到项目目录之外的任意位置。
func expandIncludes(content, baseDir, projectRoot string, seen map[string]bool, depth int) string {
	if depth > MaxIncludeDepth {
		return content
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out strings.Builder
	inCode := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		if !inCode {
			if path := parseInclude(trimmed); path != "" {
				resolved := resolveInclude(path, baseDir)
				if abs, err := filepath.Abs(resolved); err == nil && !seen[abs] {
					// @include 路径边界检查：不允许逃逸到项目目录和用户 home 之外
					if !isIncludeAllowed(abs, projectRoot) {
						out.WriteString("<!-- @include skipped: path outside project -->\n")
						continue
					}
					if data, err := os.ReadFile(abs); err == nil {
						seen[abs] = true
						out.WriteString(fmt.Sprintf("<!-- included from %s -->\n", path))
						out.WriteString(expandIncludes(string(data), filepath.Dir(abs), projectRoot, seen, depth+1))
						out.WriteByte('\n')
						continue
					}
				}
				// Fall through if include can't be resolved/read; emit the
				// original line so the user notices.
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// isIncludeAllowed 检查 @include 解析后的绝对路径是否在允许范围内。
// 允许范围：项目目录（projectRoot）及其子目录，以及用户 home 目录下的 .tencoder/。
// 这可以防止通过 @../../etc/passwd 等路径逃逸到任意位置。
func isIncludeAllowed(absPath, projectRoot string) bool {
	// 项目目录内的路径始终允许
	if projectRoot != "" && strings.HasPrefix(absPath, projectRoot+string(filepath.Separator)) {
		return true
	}
	if absPath == projectRoot {
		return true
	}
	// 用户 home 下的 .tencoder/ 目录也允许（全局指令文件的 include）
	if home, err := os.UserHomeDir(); err == nil {
		tencoderDir := filepath.Join(home, ".tencoder")
		if strings.HasPrefix(absPath, tencoderDir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// parseInclude returns the include path for a line of the form
// "@./path", "@~/path", or "@/abs/path", else "". Other @-tokens (e.g.
// @username) are ignored to avoid false positives.
func parseInclude(trimmed string) string {
	if !strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "@@") {
		return ""
	}
	rest := strings.TrimPrefix(trimmed, "@")
	if rest == "" {
		return ""
	}
	if strings.ContainsAny(rest, " \t") {
		return ""
	}
	switch {
	case strings.HasPrefix(rest, "./"), strings.HasPrefix(rest, "../"),
		strings.HasPrefix(rest, "~/"), strings.HasPrefix(rest, "/"):
		return rest
	}
	return ""
}

func resolveInclude(p, baseDir string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

// projectInstructionDirs returns directories from git root down to workDir.
// If workDir is not inside a git repo, only [workDir] is returned.
func projectInstructionDirs(workDir string) []string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return []string{workDir}
	}
	root := findGitRoot(abs)
	if root == "" {
		return []string{abs}
	}
	var dirs []string
	cur := abs
	for {
		dirs = append([]string{cur}, dirs...)
		if cur == root {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return dirs
}

func findGitRoot(start string) string {
	cur := start
	for {
		if info, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			_ = info
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}
