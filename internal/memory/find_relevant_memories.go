// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com


package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// RelevantMemory is one memory file selected for surfacing into the main conversation. MtimeMs is
// threaded through so callers can render freshness without a second stat.
type RelevantMemory struct {
	Path    string
	MtimeMs int64
}

// SelectorFn is the abstraction for the side-query LLM call used by the recall selector. Given a
// system prompt and a user message, the caller is expected to issue a one-shot model call and
// return the raw assistant text. Errors are treated as "selector failed → no recall" by
// FindRelevantMemories. Tencoder's llm.Client is a streaming + system-prompt-bound interface, so
// this callback lets the caller stand up a dedicated side-query client without coupling memory →
// llm at the package level.
type SelectorFn func(ctx context.Context, systemPrompt, userMessage string) (string, error)

// SelectMemoriesSystemPrompt is the system prompt for the selector agent. Client.
const SelectMemoriesSystemPrompt = `You are selecting memories that will be useful to Tencoder as it processes a user's query. You will be given the user's query and a list of available memory files with their filenames and descriptions.

Return a list of filenames for the memories that will clearly be useful to Tencoder as it processes the user's query (up to 5). Only include memories that you are certain will be helpful based on their name and description.
- If you are unsure if a memory will be useful in processing the user's query, then do not include it in your list. Be selective and discerning.
- If there are no memories in the list that would clearly be useful, feel free to return an empty list.
- If a list of recently-used tools is provided, do not select memories that are usage reference or API documentation for those tools (Tencoder is already exercising them). DO still select memories containing warnings, gotchas, or known issues about those tools — active use is exactly when those matter.
- Memories typed ` + "`task`" + ` record pitfalls a recent sub-task already hit, plus the recovery path that worked. They are transient (consolidated into long-term memories once the session goes idle), so they compete for the same 5 slots as everything else with no reserved quota. Give them a fair look: if the query continues, repairs, or revisits work described by a task memory, select it — a pitfall that was already paid for once is worth surfacing before it is paid for again. Their descriptions lead with the task keyword, so keyword overlap with the query is expected and is exactly the signal you want here.

Respond with valid JSON only, no markdown, in this exact shape: {"selected_memories": ["filename1.md", "filename2.md"]}`

// FindRelevantMemories scans both userMemDir and projectMemDir, asks the selector to pick up to 5
// relevant filenames for query, and returns the corresponding absolute paths + mtimes. Excludes
// MEMORY.md (already loaded in system prompt). mtime is threaded through so callers can surface
// freshness without a second stat.
//
// alreadySurfaced filters paths shown in prior turns before the selector call, so the 5-slot budget
// is spent on fresh candidates instead of re-picking files the caller will discard.
//
// Either dir may be empty — only the non-empty one is scanned. Filename collisions across the two
// dirs are disambiguated by FilePath in the returned RelevantMemory.
//
// Selector failures are silent — recall is best-effort and must never block the main conversation.
// Returns empty slice + nil error on any selector/parse error.
func FindRelevantMemories(
	ctx context.Context,
	query string,
	userMemDir, projectMemDir string,
	recentTools []string,
	alreadySurfaced map[string]struct{},
	selector SelectorFn,
) ([]RelevantMemory, error) {
	if selector == nil {
		return nil, nil
	}
	var all []MemoryHeader
	if userMemDir != "" {
		userScan, err := ScanMemoryFiles(ctx, userMemDir, "user")
		if err != nil {
			return nil, err
		}
		all = append(all, userScan...)
	}
	if projectMemDir != "" {
		projectScan, err := ScanMemoryFiles(ctx, projectMemDir, "project")
		if err != nil {
			return nil, err
		}
		all = append(all, projectScan...)
	}
	memories := make([]MemoryHeader, 0, len(all))
	for _, m := range all {
		if _, ok := alreadySurfaced[m.FilePath]; ok {
			continue
		}
		memories = append(memories, m)
	}
	if len(memories) == 0 {
		return nil, nil
	}

	selectedFilenames, _ := selectRelevantMemories(ctx, query, memories, recentTools, selector)
	byKey := make(map[string]MemoryHeader, len(memories))
	for _, m := range memories {
		byKey[m.FilePath] = m
		// Also index by Filename for selector outputs that drop the path.
		if _, exists := byKey[m.Filename]; !exists {
			byKey[m.Filename] = m
		}
		// And by bare base name, for selector outputs that flatten a nested path
		// (tasks/foo.md → foo.md). First writer wins on the rare collision between
		// two same-named files in different subdirs — a wrong pick there is still
		// surfaced, which is better than dropping a correct one.
		base := filepath.Base(m.Filename)
		if _, exists := byKey[base]; !exists {
			byKey[base] = m
		}
	}
	selected := make([]RelevantMemory, 0, len(selectedFilenames))
	for _, fn := range selectedFilenames {
		m, ok := byKey[fn]
		if !ok {
			continue
		}
		selected = append(selected, RelevantMemory{Path: m.FilePath, MtimeMs: m.MtimeMs})
	}
	return selected, nil
}

func selectRelevantMemories(
	ctx context.Context,
	query string,
	memories []MemoryHeader,
	recentTools []string,
	selector SelectorFn,
) ([]string, error) {
	// The manifest renders absolute paths (m.FilePath) but models often echo back the
	// directory-relative name instead — and for nested files like task memories the two
	// differ by more than a directory prefix (tasks/foo.md vs foo.md). Accept either form,
	// plus the bare base name, so a correct pick is never silently dropped.
	validFilenames := make(map[string]struct{}, len(memories)*3)
	for _, m := range memories {
		validFilenames[m.Filename] = struct{}{}
		if m.FilePath != "" {
			validFilenames[m.FilePath] = struct{}{}
		}
		validFilenames[filepath.Base(m.Filename)] = struct{}{}
	}

	manifest := FormatMemoryManifest(memories)

	// When Tencoder is actively using a tool (e.g. mcp__X__spawn), surfacing that tool's reference docs
	// is noise — the conversation already contains working usage. The selector otherwise matches on
	// keyword overlap ("spawn" in query + "spawn" in a memory description → false positive).
	toolsSection := ""
	if len(recentTools) > 0 {
		toolsSection = "\n\nRecently used tools: " + strings.Join(recentTools, ", ")
	}

	userMessage := fmt.Sprintf("Query: %s\n\nAvailable memories:\n%s%s", query, manifest, toolsSection)

	raw, err := selector(ctx, SelectMemoriesSystemPrompt, userMessage)
	if err != nil {
		return nil, nil
	}
	clean := extractJSONObject(raw)
	if clean == "" {
		return nil, nil
	}
	var parsed struct {
		SelectedMemories []string `json:"selected_memories"`
	}
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return nil, nil
	}
	out := make([]string, 0, len(parsed.SelectedMemories))
	for _, f := range parsed.SelectedMemories {
		if _, ok := validFilenames[f]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// extractJSONObject returns the first {.} substring found in raw, or the raw text trimmed if it
// already starts with `{`. Tolerates markdown fences or prose around the JSON despite the prompt.
func extractJSONObject(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(trimmed, "}")
	if end < start {
		return ""
	}
	return trimmed[start : end+1]
}

