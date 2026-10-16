// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 阶段一 · 任务 1.3：任务结束沉淀长期项目记忆。
//
// 决策 E（空闲后台蒸馏）的要点，全部落在这一个文件里：
//   - 空闲判定：主循环 LoopComplete 后 60 秒无新用户输入；
//   - 并发保护：全局单飞，TryLock 拿不到就跳过本轮；
//   - 沙箱对齐：写路径全部落在 ./.tencoder/memory/ 内，复用 IsAutoMemPath 放行；
//   - 蒸馏输入：待蒸馏 task 记忆全文 + 现有 project 记忆「清单」（不灌全文，控 token）；
//   - 蒸馏输出：create / update / merge / skip 四种结构化动作；
//   - 源处置：动作执行成功 → 删除源文件；失败 → 保留，下轮重试；
//   - 堆积兜底：tasks/ 超 20 条时下一轮强制触发（不受 60s 窗口约束）。
//
// 「体积不单调增长」在这里是结构性保证而不是调参结果：task 记忆只减不增（蒸馏成功即删），
// 长期记忆只经 update/merge/skip，skip 同时把可推导内容执行掉。
//
// 不设归档目录：归档只是把「单调增长」挪到另一个地方，阶段一不需要审计回溯。

// DefaultConsolidationIdleWindow is how long the session must be idle (no new user input) before
// a consolidation round runs. Overridable per session via ConsolidatorDeps.IdleWindow.
const DefaultConsolidationIdleWindow = 60 * time.Second

// MaxPendingTaskMemories is the pile-up backstop: once tasks/ holds more than this many files,
// the next round is forced regardless of the idle window. Prevents task memories from crowding
// out the ≤5 recall slots during a long uninterrupted session.
const MaxPendingTaskMemories = 20

// MaxConsolidationInputBytes caps the total task-memory text fed to the distiller in one round.
// Beyond this the rest wait for the next round — consolidation is best-effort and must stay
// bounded, otherwise one pathological round could dwarf the whole memory budget.
const MaxConsolidationInputBytes = 48 * 1024

// ConsolidatorAction is the verb of a distillation instruction. Exactly four verbs — the LLM has
// no way to express "partially useful" or "rewrite the whole memory system", which is the point.
type ConsolidatorAction string

const (
	// ActionCreate writes a brand-new long-term memory from the task memory's durable content.
	ActionCreate ConsolidatorAction = "create"
	// ActionUpdate folds the task memory's durable content into an EXISTING long-term memory,
	// preserving everything already in that memory. Never replace wholesale.
	ActionUpdate ConsolidatorAction = "update"
	// ActionMerge combines several existing long-term memories into one (dedup: the same lesson
	// recorded twice in two files). All listed files are rewritten into the survivor.
	ActionMerge ConsolidatorAction = "merge"
	// ActionSkip drops the task memory with no long-term change. This is the whitelist enforcer:
	// anything the extractor should not have written (derivable content) goes here.
	ActionSkip ConsolidatorAction = "skip"
)

// ConsolidatorDeps holds the collaborators a Consolidator needs.
type ConsolidatorDeps struct {
	ProjectRoot string
	// ConsolidatorFn is the side-query LLM call. Same injection pattern as TaskExtractorFn — the
	// memory package takes no package-level llm dependency.
	ConsolidatorFn TaskExtractorFn
	// IdleWindow overrides DefaultConsolidationIdleWindow when non-zero.
	IdleWindow time.Duration
	// Logf is optional; used for the single-line token accounting log (阶段一 T6) and for the
	// concurrency-protection notes the design doc asks to document.
	Logf func(format string, args ...any)
}

// Consolidator distills pending task memories into long-term project memories when the session
// goes idle. Safe for concurrent use.
type Consolidator struct {
	deps ConsolidatorDeps

	// inFlightMu guards the single-flight latch. sync.Mutex.TryLock is used rather than a bool
	// flag so "already running → skip this round" and the flag set happen atomically; with a plain
	// bool the check-then-set races between two triggers (idle timer + pile-up force).
	inFlightMu sync.Mutex
	inFlight   bool

	// timerMu guards timer + lastActivity. Both are needed to decide whether an armed timer is
	// still valid after a race with NotifyActivity.
	timerMu      sync.Mutex
	timer        *time.Timer
	lastActivity time.Time

	// stopOnce lets the TUI shutdown path disarm the pending idle timer exactly once.
	stopOnce sync.Once
}

// NewConsolidator constructs a Consolidator. Returns nil when no distiller is wired — callers
// can then invoke NotifyActivity / RunPending / Stop unconditionally, and nil receivers no-op.
func NewConsolidator(deps ConsolidatorDeps) *Consolidator {
	if deps.ConsolidatorFn == nil || deps.ProjectRoot == "" {
		return nil
	}
	if deps.IdleWindow <= 0 {
		deps.IdleWindow = DefaultConsolidationIdleWindow
	}
	return &Consolidator{deps: deps, lastActivity: time.Now()}
}

// NotifyActivity records activity and (re)arms the idle timer. Called on every user input and on
// LoopComplete — the semantic is "the session just became busy (or just finished a turn),
// schedule the distillation check for when it goes quiet again".
//
// A re-armed timer rather than sleep-then-check is what makes「60秒无新用户输入」real: any new
// input pushes the deadline out, so a user who keeps typing is never interrupted mid-thought by a
// consolidation round.
func (c *Consolidator) NotifyActivity() {
	if c == nil {
		return
	}
	c.timerMu.Lock()
	c.lastActivity = time.Now()
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(c.deps.IdleWindow, func() { c.runIfIdle() })
	c.timerMu.Unlock()
}

// Stop disarms the idle timer. Called from the TUI shutdown path. In-flight rounds are not
// cancelled — they hold one LLM call and are already best-effort; killing one mid-write would be
// worse than letting it finish. Any task memory it was processing stays in tasks/ for next time.
func (c *Consolidator) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		c.timerMu.Lock()
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
		c.timerMu.Unlock()
	})
}

// runIfIdle is the timer callback. Re-checks idleness: if the timer fired while the user is
// mid-turn (NotifyActivity raced the arming), re-arm for the remaining time instead of
// consolidating — a round during active work competes with the conversation for the same LLM
// budget for no benefit.
func (c *Consolidator) runIfIdle() {
	c.timerMu.Lock()
	idle := time.Since(c.lastActivity) >= c.deps.IdleWindow
	c.timerMu.Unlock()
	if !idle {
		c.NotifyActivity()
		return
	}
	c.Run(context.Background())
}

// Run performs one consolidation round and returns how many task-memory files were consumed
// (distilled or skipped). Errors are swallowed by design — the memory subsystem never blocks the
// conversation; a failed round leaves its source files in place for the next attempt.
func (c *Consolidator) Run(ctx context.Context) int {
	if c == nil {
		return 0
	}

	// Single-flight: TryLock as the first statement, so two concurrent triggers (idle timer and
	// pile-up force) produce exactly one round.
	if !c.inFlightMu.TryLock() {
		c.logf("[task-consolidate] skipped — a round is already in flight")
		return 0
	}
	defer c.inFlightMu.Unlock()

	pending, longTerm := c.loadPending()
	if len(pending) == 0 {
		return 0
	}

	start := time.Now()
	raw, usage, err := c.deps.ConsolidatorFn(ctx, ConsolidatorSystemPrompt,
		buildConsolidationMessage(pending, longTerm))
	if err != nil {
		c.logf("[task-consolidate] distill failed: err=%v dur=%s",
			err, time.Since(start).Round(time.Millisecond))
		return 0
	}

	actions := parseConsolidationActions(raw, pending)
	if len(actions) == 0 {
		// Unparseable output → keep every source. Deleting on a parse failure would silently lose
		// the only record of a real pitfall, which is the one thing this subsystem exists for.
		c.logf("[task-consolidate] no usable actions, keeping %d file(s) dur=%s",
			len(pending), time.Since(start).Round(time.Millisecond))
		return 0
	}

	consumed := 0
	for i := range actions {
		if actions[i].consume(c.deps.ProjectRoot) {
			consumed++
		}
	}
	c.logf("[task-consolidate] round done: tasks=%d consumed=%d create=%d update=%d merge=%d skip=%d in=%d out=%d dur=%s",
		len(pending), consumed,
		countAction(actions, ActionCreate), countAction(actions, ActionUpdate),
		countAction(actions, ActionMerge), countAction(actions, ActionSkip),
		usage.InputTokens, usage.OutputTokens, time.Since(start).Round(time.Millisecond))

	if len(pending) > MaxPendingTaskMemories {
		// Pile-up: force a follow-up round rather than waiting for the idle window. Re-arm via
		// the timer instead of recursing — a direct recursive call would hit our own TryLock and
		// no-op, since we still hold the latch here.
		c.NotifyActivity()
	}
	return consumed
}

// PendingTaskMemory is one staging file awaiting consolidation.
type PendingTaskMemory struct {
	FilePath    string // absolute
	Filename    string // path relative to the memory dir, e.g. "tasks/foo.md" — what the LLM echoes
	Description string
	Content     string
}

// LongTermMemoryRef is one existing long-term memory as the distiller sees it: manifest level only.
type LongTermMemoryRef struct {
	Filename    string
	Description string
}

type consolidationInstruction struct {
	TaskFile    string             `json:"task_file"`
	Action      ConsolidatorAction `json:"action"`
	TargetFile  string             `json:"target_file"`
	Content     string             `json:"content"`
	DeleteFiles []string           `json:"delete_files"`

	// source is the resolved staging path, filled in by parseConsolidationActions. Kept out of the
	// JSON so the model never has to echo an absolute path correctly.
	source string
}

// consume executes the instruction and reports whether its task-memory source may be deleted.
//
// The delete is the last step and happens only when everything before it succeeded — the design's
// 「动作执行成功 → 删除源文件；失败 → 保留待下轮重试」. skip is always a success: dropping the file
// without touching long-term memory is a complete, valid outcome.
func (in *consolidationInstruction) consume(projectRoot string) bool {
	if in.source == "" {
		return false
	}
	switch in.Action {
	case ActionSkip:
		return removeTaskMemory(in.source)

	case ActionCreate, ActionUpdate:
		target, ok := resolveMemoryFile(projectRoot, in.TargetFile)
		if !ok {
			return false
		}
		if in.Action == ActionUpdate && !fileExists(target) {
			// The manifest lied, or the file was deleted since. Creating it instead would be a
			// reasonable repair, but silently converting update → create hides a real
			// inconsistency; keep the source and let the next round retry.
			return false
		}
		if err := applyMemoryEdit(target, in.Content, in.Action == ActionUpdate); err != nil {
			return false
		}
		return removeTaskMemory(in.source)

	case ActionMerge:
		survivor, ok := resolveMemoryFile(projectRoot, in.TargetFile)
		if !ok {
			return false
		}
		// merge is the ONE case where a full replacement is written: collapsing duplicates is the
		// whole point, and the distiller was given the manifest to justify it.
		if err := applyMemoryEdit(survivor, in.Content, false); err != nil {
			return false
		}
		// Only delete merge targets after the survivor is safely written, and only paths that
		// actually exist as long-term files — an LLM must not be able to name an arbitrary file
		// for removal.
		for _, victim := range in.DeleteFiles {
			if abs, ok := resolveMemoryFile(projectRoot, victim); ok && abs != survivor && fileExists(abs) {
				_ = os.Remove(abs)
			}
		}
		return removeTaskMemory(in.source)

	default:
		return false
	}
}

// ConsolidatorSystemPrompt is the distiller's system prompt. It states the four-action contract,
// the「长期记忆存世界的状态 / 任务记忆存教训」split (the interview narrative anchor), and that skip
// is a first-class outcome rather than a failure.
const ConsolidatorSystemPrompt = `You consolidate task memories into long-term project memories.

Long-term memories store THE STATE OF THE WORLD — durable facts, decisions, constraints, and gotchas that hold beyond the task that produced them. Task memories store WHAT WAS LEARNED WHILE WORKING — a pitfall plus the recovery path. Your job is to move the durable half of the lesson into the long-term layer and let the rest die with the task memory.

You will be given:
- The full text of the pending task memories (transient staging records; these files are deleted after this round).
- A manifest of existing long-term project memories: filename + description only. You cannot read their bodies, so if you must modify one, describe the edit as an instruction rather than a replacement of unknown content.

Choose exactly one action per task memory:

- "create" — nothing suitable exists yet. Give a new filename and the full new memory body.
- "update" — an existing memory is the right home but is incomplete. Give its filename and ONLY the content to ADD or correct. Never restate or drop what is already there; you cannot see it.
- "merge" — several listed memories record the same lesson and should be one file. Give the surviving filename, the filenames to delete, and the merged body (this is the ONE case where you may write a full replacement, because you are collapsing duplicates).
- "skip" — nothing durable here. The pitfall was situation-specific, or it is derivable from the current code (a fix recipe, a file inventory, an architecture note — if grep or git would find it, it does not belong in memory). Skipping is a normal, correct outcome and is the expected answer for most task memories.

Rules:
- One action per task memory. Never leave a task memory unaddressed.
- Do not invent content. If a task memory states no cause and no recovery path, skip it.
- A user's verbal constraint ("never touch the vendored dir") IS durable — create or update for it, even though no code records it.
- Hidden coupling ("changing A requires updating B") IS durable — keep it.

Respond with valid JSON only, no markdown:
{"actions": [{"task_file": "tasks/hardcoded-fixture-path.md", "action": "create", "target_file": "fixture-paths.md", "content": "<full markdown memory incl. frontmatter>"}]}

For "update", "content" holds only the added/corrected portion. For "merge", "content" is the full merged body and "delete_files" lists filenames to remove. For "skip", omit content.`

// loadPending reads the pending task memories (newest first, byte-capped) and the long-term
// manifest.
func (c *Consolidator) loadPending() ([]PendingTaskMemory, []LongTermMemoryRef) {
	memDir := GetAutoMemPath(c.deps.ProjectRoot)
	taskDir := GetTaskMemPath(c.deps.ProjectRoot)
	if taskDir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, nil
	}

	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, nil
	}
	// Newest first: consolidation should drain the freshest lessons (still relevant to the work in
	// progress), so the byte cap trims the oldest — the correct side to drop.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })

	var pending []PendingTaskMemory
	total := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if total >= MaxConsolidationInputBytes {
			break
		}
		path := filepath.Join(taskDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		total += len(data)
		pending = append(pending, PendingTaskMemory{
			FilePath:    path,
			Filename:    TaskMemDirName + "/" + e.Name(),
			Description: parseFrontmatter(string(data)).Description,
			Content:     string(data),
		})
	}

	// Long-term manifest: filenames + descriptions from the memory ROOT only. Subdirectories are
	// skipped, which is what keeps task memories out of the manifest — feeding them back in would
	// grow the staging area instead of draining it.
	var refs []LongTermMemoryRef
	if rootEntries, err := os.ReadDir(memDir); err == nil {
		for _, e := range rootEntries {
			if e.IsDir() || e.Name() == AutoMemEntrypointName || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(memDir, e.Name()))
			if err != nil {
				continue
			}
			refs = append(refs, LongTermMemoryRef{
				Filename:    e.Name(),
				Description: parseFrontmatter(string(data)).Description,
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Filename < refs[j].Filename })
	return pending, refs
}

type consolidationResponse struct {
	Actions []consolidationInstruction `json:"actions"`
}

var multiLineCleaner = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

// parseConsolidationActions parses the distiller's reply and resolves each action's task_file back
// to a staging path. Instructions naming an unknown task file, an unknown action, or a missing
// target are dropped: the model works from a manifest, and a hallucinated name must never make us
// delete or overwrite anything.
func parseConsolidationActions(raw string, pending []PendingTaskMemory) []consolidationInstruction {
	clean := extractJSONObject(raw)
	if clean == "" {
		return nil
	}
	var parsed consolidationResponse
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return nil
	}

	// Index by bare name, dir-relative name, and absolute path so any of the three forms the model
	// might echo resolves. Absolute paths only match entries we actually read.
	byName := make(map[string]string, len(pending)*3)
	for _, p := range pending {
		byName[p.FilePath] = p.FilePath
		byName[p.Filename] = p.FilePath
		byName[filepath.Base(p.Filename)] = p.FilePath
	}

	out := make([]consolidationInstruction, 0, len(parsed.Actions))
	seen := make(map[string]bool, len(pending))
	for _, in := range parsed.Actions {
		src, ok := byName[strings.TrimSpace(in.TaskFile)]
		if !ok {
			continue
		}
		if seen[src] {
			// One action per task memory: the first wins, later duplicates are ignored so a
			// repeated instruction cannot delete the same source twice.
			continue
		}
		switch in.Action {
		case ActionCreate, ActionUpdate, ActionMerge, ActionSkip:
		default:
			continue
		}
		in.source = src
		in.Content = strings.TrimSpace(in.Content)
		seen[src] = true
		out = append(out, in)
	}
	return out
}

func countAction(actions []consolidationInstruction, a ConsolidatorAction) int {
	n := 0
	for _, in := range actions {
		if in.Action == a {
			n++
		}
	}
	return n
}

// resolveMemoryFile maps a bare filename to an absolute path inside the project memory dir,
// rejecting anything that could escape it. The distiller only ever names a bare filename, but this
// function is the boundary between model output and the filesystem, so containment is checked
// explicitly rather than trusted.
func resolveMemoryFile(projectRoot, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", false
	}
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	return filepath.Join(GetAutoMemPath(projectRoot), name), true
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// applyMemoryEdit writes distilled content to target. update mode appends under a marker instead of
// overwriting, because the distiller never saw the existing body — overwriting would destroy
// content it could not have accounted for. merge mode replaces (collapsing duplicates is the point).
func applyMemoryEdit(target, content string, appendOnly bool) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("empty content for %s", filepath.Base(target))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if !appendOnly {
		return os.WriteFile(target, []byte(content+"\n"), 0o644)
	}
	existing, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n<!-- consolidated from a task memory -->\n")
	b.WriteString(content)
	b.WriteString("\n")
	return os.WriteFile(target, []byte(b.String()), 0o644)
}

// removeTaskMemory deletes one staging file. A "not exist" outcome counts as success — another
// round may have already drained it.
func removeTaskMemory(path string) bool {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false
	}
	return true
}

// buildConsolidationMessage assembles the distiller input: pending task-memory bodies in full,
// plus the long-term manifest (filename + description only — full bodies would blow the token
// budget for no accuracy gain, since the distiller's job is routing, not rewriting).
func buildConsolidationMessage(pending []PendingTaskMemory, longTerm []LongTermMemoryRef) string {
	var b strings.Builder
	b.WriteString("## Pending task memories (transient — these files are deleted after this round)\n\n")
	for _, p := range pending {
		fmt.Fprintf(&b, "### %s\n", p.Filename)
		if p.Description != "" {
			fmt.Fprintf(&b, "description: %s\n\n", multiLineCleaner.Replace(p.Description))
		}
		b.WriteString(p.Content)
		b.WriteString("\n\n")
	}

	b.WriteString("## Existing long-term project memories (manifest only — bodies not shown)\n\n")
	if len(longTerm) == 0 {
		b.WriteString("(none yet — every durable lesson becomes a \"create\")\n")
	} else {
		for _, r := range longTerm {
			fmt.Fprintf(&b, "- %s", r.Filename)
			if r.Description != "" {
				fmt.Fprintf(&b, ": %s", multiLineCleaner.Replace(r.Description))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (c *Consolidator) logf(format string, args ...any) {
	if c.deps.Logf != nil {
		c.deps.Logf(format, args...)
	}
}
