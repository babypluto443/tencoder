// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// AutoMemEntrypointName is the filename of the per-project memory index.
const AutoMemEntrypointName = "MEMORY.md"

// GetAutoMemPath returns the auto-memory directory path for the given
// project root. Shape: <projectRoot>/.tencoder/memory/
//
// The trailing separator is preserved so prefix-based path matching (e.g.,
// sandbox `HasPrefix` checks) work correctly without falsely matching
// `…/memoryxyz`.
//
// Tencoder colocates memory with other project-local state under .tencoder/
// so records show up in the IDE and editors can open them directly.
//
// Resolution order:
//  1. TENCODER_REMOTE_MEMORY_DIR env var — used as-is (escape hatch for
//     CI/container scenarios where memory should live elsewhere)
//  2. <projectRoot>/.tencoder/memory
func GetAutoMemPath(projectRoot string) string {
	if override := os.Getenv("TENCODER_REMOTE_MEMORY_DIR"); override != "" {
		return strings.TrimRight(override, string(filepath.Separator)) + string(filepath.Separator)
	}
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot
	}
	return filepath.Join(abs, ".tencoder", "memory") + string(filepath.Separator)
}

// GetAutoMemEntrypoint returns the path to the MEMORY.md inside the
// auto-memory directory.
func GetAutoMemEntrypoint(projectRoot string) string {
	dir := GetAutoMemPath(projectRoot)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// TaskMemDirName is the subdirectory of the project-level memory dir that holds
// task memories (type: task) — the transient staging area between "a sub-task
// finished" and "idle consolidation folded the durable parts into long-term
// project memories".
const TaskMemDirName = "tasks"

// GetTaskMemPath returns the task-memory directory for a project root:
// <projectRoot>/.tencoder/memory/tasks/ (trailing separator preserved, same
// convention as GetAutoMemPath).
//
// Task memories are always project-scoped: a pitfall recovered in one repo has
// no meaning in another, so there is no user-level task dir. The
// TENCODER_REMOTE_MEMORY_DIR override still applies (it overrides the whole
// memory tree), because that escape hatch exists for CI/container setups where
// the project dir itself is not writable.
func GetTaskMemPath(projectRoot string) string {
	base := GetAutoMemPath(projectRoot)
	if base == "" {
		return ""
	}
	return filepath.Join(base, TaskMemDirName) + string(filepath.Separator)
}

// EnsureTaskMemDirExists creates the task-memory directory (and parents). Called
// before writing a task memory so the write never has to reason about mkdir.
// Failures are non-fatal — the caller's write will surface a real error.
func EnsureTaskMemDirExists(projectRoot string) error {
	dir := GetTaskMemPath(projectRoot)
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// IsTaskMemPath checks if an absolute path lives inside the task-memory staging
// dir. Used by consolidation to decide which files are eligible for distillation
// and which are long-term (consolidation must never feed task memories back in).
func IsTaskMemPath(absolutePath, projectRoot string) bool {
	dir := GetTaskMemPath(projectRoot)
	if dir == "" {
		return false
	}
	abs := filepath.Clean(absolutePath)
	return strings.HasPrefix(abs+string(filepath.Separator), dir)
}

// IsAutoMemPath checks if an absolute path is within EITHER the project-level
// or user-level auto-memory directory. Used by the path sandbox to allow
// Writes into either memory dir.
//
// Note this is a prefix test against the memory root, so the tasks/ subdir is
// covered for free — task memories need no separate sandbox exemption.
func IsAutoMemPath(absolutePath, projectRoot string) bool {
	abs := filepath.Clean(absolutePath)
	if dir := GetAutoMemPath(projectRoot); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	if dir := GetUserAutoMemPath(); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	return false
}

// GetUserAutoMemPath returns the user-level auto-memory directory:
// ~/.tencoder/memory/. Used for type=user / type=feedback memories that
// follow the human across projects (e.g. coding preferences). Returns ""
// if the home directory cannot be resolved.
//
// Trailing separator is preserved for prefix-based path matching.
func GetUserAutoMemPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".tencoder", "memory") + string(filepath.Separator)
}

// GetUserAutoMemEntrypoint returns the path to ~/.tencoder/memory/MEMORY.md.
func GetUserAutoMemEntrypoint() string {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// IsUserAutoMemPath checks if an absolute path is within the user-level
// memory dir. Used in places where we need to distinguish user-scope from
// project-scope (sandbox already accepts both; this is for routing).
func IsUserAutoMemPath(absolutePath string) bool {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return false
	}
	abs := filepath.Clean(absolutePath)
	return strings.HasPrefix(abs+string(filepath.Separator), dir)
}
