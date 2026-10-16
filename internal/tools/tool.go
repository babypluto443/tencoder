// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package tools

import (
	"context"
	"strings"
	"sync"
)

var SkipDirs = map[string]bool{
	".git": true, ".venv": true, "node_modules": true,
	"__pycache__": true, ".tox": true, ".mypy_cache": true,
}

const MaxOutputChars = 10000

type ToolResult struct {
	Output  string
	IsError bool
}

type ToolCategory string

const (
	CategoryRead    ToolCategory = "read"
	CategoryWrite   ToolCategory = "write"
	CategoryCommand ToolCategory = "command"
)

type Tool interface {
	Name() string
	Description() string
	Category() ToolCategory
	Schema() map[string]any
	Execute(ctx context.Context, args map[string]any) ToolResult
}

type DeferrableTool interface {
	ShouldDefer() bool
}

// SystemTool marks tools that operate on the agent's own state (e.g.
// LoadSkill) rather than on external resources. System tools bypass the
// per-skill allowed_tools whitelist applied via Agent.ToolNameFilter so
// that skills can always delegate to other skills regardless of how
// narrowly the current skill restricted its visible tool set.
type SystemTool interface {
	IsSystemTool() bool
}

// IsSystemTool returns true if t opts into the SystemTool contract and
// reports itself as a system tool. Concentrated here so callers don't
// duplicate the type-assert dance.
func IsSystemTool(t Tool) bool {
	st, ok := t.(SystemTool)
	return ok && st.IsSystemTool()
}

// Registry holds the tool set plus the deferred-loading state.
//
// 阶段三 made this concurrent-safe: tools execute on StreamingExecutor goroutines while the main
// loop reads schemas and marks discoveries, and the eviction bookkeeping (eviction.go) is written
// from the execution path. The previous bare maps were only accidentally safe because nothing
// touched them from a goroutine; that is no longer true.
//
// mu guards tools / discoveredTools / usage / evictionPolicy as one unit. Schemas are computed
// OUTSIDE the lock in GetAllSchemas (see below) because Tool.Schema() can be non-trivial and
// holding the registry lock across it would serialize every tool call in the turn.
type Registry struct {
	mu              sync.RWMutex
	tools           map[string]Tool
	discoveredTools map[string]bool
	usage           map[string]ToolUsageStats
	evictionPolicy  EvictionPolicy
}

func NewRegistry() *Registry {
	return &Registry{
		tools:           make(map[string]Tool),
		discoveredTools: make(map[string]bool),
		usage:           make(map[string]ToolUsageStats),
		evictionPolicy:  DefaultEvictionPolicy(),
	}
}

// Register adds or replaces a tool. Registration happens during bootstrap (single-threaded), but
// it is locked anyway so a late MCP connect can't race a reader.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	r.tools[t.Name()] = t
	r.mu.Unlock()
}

func (r *Registry) Get(name string) Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

func (r *Registry) ListTools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

func isDeferred(t Tool) bool {
	if dt, ok := t.(DeferrableTool); ok {
		return dt.ShouldDefer()
	}
	return false
}

func isOpenAIProtocol(protocol string) bool {
	return protocol == "openai" || protocol == "openai-compat"
}

// GetAllSchemas renders the resident tool set for one API request.
//
// The two-phase shape (snapshot under RLock → render outside) matters: Tool.Schema() builds a
// fresh map per tool, and doing that while holding the lock would block every concurrent
// RecordExecute for the duration of rendering every schema — which is exactly when tool
// executions are in flight.
func (r *Registry) GetAllSchemas(protocol string) []map[string]any {
	r.mu.RLock()
	resident := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		if isDeferred(t) && !r.discoveredTools[t.Name()] {
			continue
		}
		resident = append(resident, t)
	}
	r.mu.RUnlock()

	schemas := make([]map[string]any, 0, len(resident))
	for _, t := range resident {
		base := t.Schema()
		if isOpenAIProtocol(protocol) {
			schemas = append(schemas, map[string]any{
				"type":        "function",
				"name":        base["name"],
				"description": base["description"],
				"parameters":  base["input_schema"],
			})
		} else {
			schemas = append(schemas, base)
		}
	}
	return schemas
}

// GetDeferredToolNames lists the tools still parked in the deferred pool — including any that were
// evicted during this session, which is what makes eviction lossless: an evicted tool reappears
// here and ToolSearch can load it again.
func (r *Registry) GetDeferredToolNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for _, t := range r.tools {
		if isDeferred(t) && !r.discoveredTools[t.Name()] {
			names = append(names, t.Name())
		}
	}
	return names
}

func (r *Registry) GetDeferredTools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []Tool
	for _, t := range r.tools {
		if isDeferred(t) {
			result = append(result, t)
		}
	}
	return result
}

func (r *Registry) SearchDeferred(query string, maxResults int, protocol string) []map[string]any {
	query = strings.ToLower(query)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matches []map[string]any
	for _, t := range r.tools {
		if !isDeferred(t) {
			continue
		}
		name := strings.ToLower(t.Name())
		desc := strings.ToLower(t.Description())
		if strings.Contains(name, query) || strings.Contains(desc, query) {
			base := t.Schema()
			if isOpenAIProtocol(protocol) {
				matches = append(matches, map[string]any{
					"type":        "function",
					"name":        base["name"],
					"description": base["description"],
					"parameters":  base["input_schema"],
				})
			} else {
				matches = append(matches, base)
			}
			if len(matches) >= maxResults {
				break
			}
		}
	}
	return matches
}

func (r *Registry) FindDeferredByNames(names []string, protocol string) []map[string]any {
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[strings.ToLower(n)] = true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matches []map[string]any
	for _, t := range r.tools {
		if nameSet[strings.ToLower(t.Name())] {
			base := t.Schema()
			if isOpenAIProtocol(protocol) {
				matches = append(matches, map[string]any{
					"type":        "function",
					"name":        base["name"],
					"description": base["description"],
					"parameters":  base["input_schema"],
				})
			} else {
				matches = append(matches, base)
			}
		}
	}
	return matches
}

type DefaultTools struct {
	Registry  *Registry
	WriteFile *WriteFileTool
	EditFile  *EditFileTool
}

func CreateDefaultRegistry() *Registry {
	dt := CreateDefaultTools()
	return dt.Registry
}

func CreateDefaultToolsWithWorkDir(workDir string) DefaultTools {
	fsc := NewFileStateCache()
	wf := &WriteFileTool{FileStateCache: fsc}
	ef := &EditFileTool{FileStateCache: fsc}
	reg := NewRegistry()
	reg.Register(&ReadFileTool{FileStateCache: fsc})
	reg.Register(wf)
	reg.Register(ef)
	reg.Register(&BashTool{WorkDir: workDir})
	reg.Register(&GlobTool{})
	reg.Register(&GrepTool{})
	return DefaultTools{Registry: reg, WriteFile: wf, EditFile: ef}
}

func CreateDefaultTools() DefaultTools {
	return CreateDefaultToolsWithWorkDir("")
}
