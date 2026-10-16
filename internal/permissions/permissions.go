// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package permissions

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"tencoder/internal/tools"
)

// splitCompoundCommand 拆分 shell 复合命令（&&、||、;、|）为独立子命令。
// 对齐 Claude Code checkSandboxAutoAllow 的子命令拆分逻辑，防止
// "echo ok && rm -rf /" 绕过针对 rm 的 deny 规则。
func splitCompoundCommand(cmd string) []string {
	parts := regexp.MustCompile(`\s*(?:&&|\|\||[;|])\s*`).Split(cmd, -1)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return []string{cmd}
	}
	return result
}

type DecisionEffect string

const (
	Allow DecisionEffect = "allow"
	Deny  DecisionEffect = "deny"
	Ask   DecisionEffect = "ask"
)

type Decision struct {
	Effect DecisionEffect
	Reason string

	// ReadonlyWrite marks "this Ask exists because the target is in a read-only workspace".
	//
	// The TUI reads it to DISABLE the "always allow" option for this one prompt. That matters:
	// granting always-allow on a readonly project silently converts it into a writable one for
	// the rest of the session, which is exactly the 手滑 this escalation is meant to prevent.
	ReadonlyWrite bool
	// Workspace names the registered project involved, for the prompt text and the audit record.
	Workspace string
}

type PermissionMode string

const (
	ModeDefault     PermissionMode = "default"
	ModeAcceptEdits PermissionMode = "acceptEdits"
	ModePlan        PermissionMode = "plan"
	ModeBypass      PermissionMode = "bypassPermissions"
)

var modeMatrix = map[PermissionMode]map[tools.ToolCategory]DecisionEffect{
	ModeDefault:     {tools.CategoryRead: Allow, tools.CategoryWrite: Ask, tools.CategoryCommand: Ask},
	ModeAcceptEdits: {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Ask},
	ModeBypass:      {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Allow},
}

func ModeDecide(mode PermissionMode, category tools.ToolCategory) DecisionEffect {
	m, ok := modeMatrix[mode]
	if !ok {
		return Ask
	}
	return m[category]
}

// Layer 1: Dangerous command detection

type dangerousPattern struct {
	re     *regexp.Regexp
	reason string
}

var defaultDangerousPatterns = []dangerousPattern{
	{regexp.MustCompile(`rm\s+-[a-z]*r[a-z]*f[a-z]*\s+/\s*$`), "recursive force delete root"},
	{regexp.MustCompile(`mkfs\.`), "format disk"},
	{regexp.MustCompile(`dd\s+if=.*of=/dev/`), "direct write to disk device"},
	{regexp.MustCompile(`chmod\s+-R\s+777\s+/`), "recursive chmod root"},
	{regexp.MustCompile(`:\(\)\{\s*:\|:&\s*\};:`), "fork bomb"},
	{regexp.MustCompile(`curl\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`wget\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`>\s*/dev/sd`), "overwrite disk device"},
	// Git 破坏性命令——防止误操作丢失工作
	{regexp.MustCompile(`git\s+push\s+.*--force`), "force push"},
	{regexp.MustCompile(`git\s+reset\s+--hard`), "hard reset"},
	{regexp.MustCompile(`git\s+clean\s+-f`), "force clean untracked files"},
	{regexp.MustCompile(`git\s+checkout\s+\.`), "discard all changes"},
	{regexp.MustCompile(`git\s+branch\s+-D`), "force delete branch"},
}

func DetectDangerous(command string) (bool, string) {
	for _, p := range defaultDangerousPatterns {
		if p.re.MatchString(command) {
			return true, p.reason
		}
	}
	return false, ""
}

// Layer 2: Path sandbox

type PathSandbox struct {
	allowedRoots  []string
	readonlyRoots []string
	denyWrite     []string // 始终只读的受保护路径，优先级高于 allowedRoots
	// rootNames maps an absolute root to a human-readable project name, so a refusal can say
	// "pluto_feed is read-only" instead of dumping a filesystem path at the user.
	rootNames map[string]string
	// explicitWritable marks roots the USER registered as writable (AddWritableRoot), as opposed to
	// the implicit allowances NewPathSandbox adds (the project root and the OS temp dir).
	//
	// 阶段四: this distinction exists because the temp-dir allowance is a blanket scratch-space
	// grant, and it must not be able to silently defeat an explicit "this project is read-only"
	// registration. Before this, registering a project under /tmp as readonly had no effect at
	// all — the temp root matched first and the write was allowed.
	explicitWritable map[string]bool
}

// NewPathSandbox 创建路径沙箱。denyWrite 指定受保护路径（如配置文件），
// 即使在 allowedRoots 内也拒绝写入。
func NewPathSandbox(projectRoot string, extraAllowed ...string) *PathSandbox {
	// Every root is symlink-resolved at construction. Combined with resolveSandboxPath on the
	// check side, this makes every comparison resolved-to-resolved — without it a root stored as
	// /var/... never matches a target resolved as /private/var/..., and the sandbox denies
	// legitimate paths while allowing others.
	root, _ := resolveSandboxPath(projectRoot)
	if root == "" {
		root, _ = filepath.Abs(projectRoot)
	}
	allowed := []string{root}
	if tmp, err := resolveSandboxPath(os.TempDir()); err == nil {
		allowed = append(allowed, tmp)
	} else {
		allowed = append(allowed, filepath.Clean(os.TempDir()))
	}
	for _, p := range extraAllowed {
		if abs, err := resolveSandboxPath(p); err == nil {
			allowed = append(allowed, abs)
		}
	}

	// 默认受保护路径：防止 Agent 篡改权限配置和 Skill 定义
	// 注意用 filepath.Clean 而非 EvalSymlinks：这些文件此刻通常不存在，
	// denyWrite 里的路径同样会在 CheckWrite 里被 resolveSandboxPath 解析到同一形态。
	denyWrite := []string{
		filepath.Clean(filepath.Join(root, ".tencoder", "config.yaml")),
		filepath.Clean(filepath.Join(root, ".tencoder", "permissions.local.yaml")),
		filepath.Clean(filepath.Join(root, ".tencoder", "skills")),
	}

	return &PathSandbox{
		allowedRoots:     allowed,
		denyWrite:        denyWrite,
		rootNames:        map[string]string{},
		explicitWritable: map[string]bool{},
	}
}

// AddReadonlyRoot registers a read-only project root.
//
// The name appears verbatim in refusal messages. An empty name degrades to showing the path —
// worse, but still a refusal, never a security hole.
func (s *PathSandbox) AddReadonlyRoot(path, name string) {
	abs, err := resolveSandboxPath(path)
	if err != nil {
		return
	}
	s.readonlyRoots = append(s.readonlyRoots, abs)
	if s.rootNames == nil {
		s.rootNames = map[string]string{}
	}
	if name != "" {
		s.rootNames[abs] = name
	}
}

// GetReadonlyRoots 返回已注册的只读工作区列表。
func (s *PathSandbox) GetReadonlyRoots() []string { return s.readonlyRoots }

// withinRootPath 报告 abs 是否等于 root 或位于 root 之下。
//
// 用带分隔符后缀的前缀比较，而不是裸 strings.HasPrefix ——后者会认为
// "/repo-evil" 在 "/repo" 之内，这是最经典的沙箱逃逸。
func withinRootPath(abs, root string) bool {
	abs = filepath.Clean(abs)
	root = filepath.Clean(root)
	if abs == root {
		return true
	}
	return strings.HasPrefix(abs, root+string(filepath.Separator))
}

// CheckRead 校验读权限：allowedRoots ∪ readonlyRoots。
func (s *PathSandbox) CheckRead(path string) (bool, string) {
	abs, err := resolveSandboxPath(path)
	if err != nil {
		return false, fmt.Sprintf("cannot resolve path: %s", path)
	}
	for _, root := range append(append([]string{}, s.allowedRoots...), s.readonlyRoots...) {
		if withinRootPath(abs, root) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("path %s outside all registered workspaces", path)
}

// CheckWrite 校验写权限：仅 allowedRoots —— readonlyRoots 被刻意排除。
//
// 返回 false 在 Checker.Check 里**不是硬拒绝**：命中只读工作区的写会升级为 Ask 且
// 禁用 AllowAlways，让用户可以单次批准而不永久授予写权限。
func (s *PathSandbox) CheckWrite(path string) (bool, string) {
	abs, err := resolveSandboxPath(path)
	if err != nil {
		return false, fmt.Sprintf("cannot resolve path: %s", path)
	}

	// 受保护路径优先检查：命中则直接拒绝（任何级别都不行）
	for _, deny := range s.denyWrite {
		if abs == deny || strings.HasPrefix(abs, deny+string(filepath.Separator)) {
			return false, fmt.Sprintf("protected path: %s", path)
		}
	}

	// Precedence, in order (阶段四 4.2):
	//  1. an explicitly-registered WRITABLE root always wins;
	//  2. an explicit read-only registration beats the implicit scratch-space allowance;
	//  3. only then do the implicit roots (project root, temp dir) apply.
	//
	// Without step 2 the temp-dir allowance would void every read-only registration made under
	// /tmp — the user would register a project as read-only and watch writes succeed.
	for _, root := range s.allowedRoots {
		if withinRootPath(abs, root) && s.explicitWritable[root] {
			return true, ""
		}
	}
	if name := s.readonlyOwner(abs); name != "" {
		return false, fmt.Sprintf("read-only workspace %s", name)
	}
	for _, root := range s.allowedRoots {
		if withinRootPath(abs, root) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("path %s outside all registered workspaces", path)
}

// ReadonlyViolation 报告一次写是否命中只读工作区，并返回该工作区名。
//
// 与 CheckWrite 分开，是因为 Checker 需要区分**三种**结果
// —— 在可写区内 / 在只读区内 / 完全在范围外 —— 而 bool+string 承载不了第三种。
func (s *PathSandbox) ReadonlyViolation(path string) (bool, string) {
	abs, err := resolveSandboxPath(path)
	if err != nil {
		return false, ""
	}
	// 与 CheckWrite 同一套优先级：显式注册的 writable 优先于只读；隐式的临时目录不优先于只读。
	for _, root := range s.allowedRoots {
		if withinRootPath(abs, root) && s.explicitWritable[root] {
			return false, ""
		}
	}
	name := s.readonlyOwner(abs)
	if name != "" {
		return true, name
	}
	return false, ""
}

// readonlyOwner 返回包含 abs 的只读工作区名，未命中返回 ""。
func (s *PathSandbox) readonlyOwner(abs string) string {
	for _, root := range s.readonlyRoots {
		if withinRootPath(abs, root) {
			return s.rootNames[root]
		}
	}
	return ""
}

// Check 保留原签名，语义等价于 CheckWrite（历史调用方按写语义使用）。
func (s *PathSandbox) Check(path string) (bool, string) {
	return s.CheckWrite(path)
}

// GetDenyWrite 返回受保护路径列表，供沙箱 Config 构建使用
func (s *PathSandbox) GetDenyWrite() []string {
	return s.denyWrite
}

// GetAllowedRoots 返回允许写入的路径列表，供沙箱 Config 构建使用
func (s *PathSandbox) GetAllowedRoots() []string {
	return s.allowedRoots
}

// Layer 3: Rule engine

type RuleEffect string

const (
	RuleAllow RuleEffect = "allow"
	RuleDeny  RuleEffect = "deny"
	RuleAsk   RuleEffect = "ask"
)

type Rule struct {
	ToolName string
	Pattern  string
	Effect   RuleEffect
}

func (r Rule) Matches(toolName, content string) bool {
	if r.ToolName != toolName {
		return false
	}
	// 简单通配符匹配：* 匹配任意字符（包括 /），适用于 Bash 命令等非路径场景。
	// filepath.Match 的 * 不匹配 /，导致 "allow always" 对含路径的命令失效。
	return globMatch(r.Pattern, content)
}

// globMatch 实现简单的通配符匹配，* 匹配任意字符（包括 /）。
func globMatch(pattern, content string) bool {
	// 快捷路径：无通配符时做精确比较
	if !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
		return pattern == content
	}
	// 将 pattern 转为正则：* → .*, ? → .
	re := "^"
	for _, ch := range pattern {
		switch ch {
		case '*':
			re += ".*"
		case '?':
			re += "."
		case '.', '+', '^', '$', '{', '}', '(', ')', '|', '[', ']', '\\':
			re += "\\" + string(ch)
		default:
			re += string(ch)
		}
	}
	re += "$"
	matched, _ := regexp.MatchString(re, content)
	return matched
}

type RuleEngine struct {
	UserPath    string
	ProjectPath string
	LocalPath   string
}

func (e *RuleEngine) Evaluate(toolName, content string) *RuleEffect {
	for _, path := range []string{e.UserPath, e.ProjectPath, e.LocalPath} {
		rules := loadRulesFile(path)
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].Matches(toolName, content) {
				eff := rules[i].Effect
				return &eff
			}
		}
	}
	return nil
}

func (e *RuleEngine) AppendLocalRule(r Rule) {
	if e.LocalPath == "" {
		return
	}
	os.MkdirAll(filepath.Dir(e.LocalPath), 0o755)
	rules := loadRulesFile(e.LocalPath)
	rules = append(rules, r)
	var entries []map[string]string
	for _, rule := range rules {
		entries = append(entries, map[string]string{
			"rule":   fmt.Sprintf("%s(%s)", rule.ToolName, rule.Pattern),
			"effect": string(rule.Effect),
		})
	}
	data, _ := yaml.Marshal(entries)
	os.WriteFile(e.LocalPath, data, 0o644)
}

func loadRulesFile(path string) []Rule {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []struct {
		RuleStr string `yaml:"rule"`
		Effect  string `yaml:"effect"`
	}
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var rules []Rule
	for _, e := range entries {
		if e.Effect != "allow" && e.Effect != "deny" && e.Effect != "ask" {
			continue
		}
		r, err := parseRule(e.RuleStr, RuleEffect(e.Effect))
		if err != nil {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

var ruleRE = regexp.MustCompile(`^(\w+)\((.+)\)$`)

func parseRule(raw string, effect RuleEffect) (Rule, error) {
	m := ruleRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return Rule{}, fmt.Errorf("invalid rule syntax: %s", raw)
	}
	return Rule{ToolName: m[1], Pattern: m[2], Effect: effect}, nil
}

// Safe read-only commands that don't need permission

var safeCommandPrefixes = []string{
	"ls", "dir", "pwd", "echo", "cat", "head", "tail", "wc",
	"find", "which", "whereis", "whoami", "hostname", "uname",
	"date", "cal", "uptime", "df", "du", "free", "env", "printenv",
	"file", "stat", "readlink", "realpath", "basename", "dirname",
	"sort", "uniq", "tr", "cut", "awk", "sed", "grep", "egrep", "fgrep",
	"diff", "comm", "tee", "xargs", "true", "false", "test",
	"git status", "git log", "git diff", "git show", "git branch",
	"git tag", "git remote", "git rev-parse", "git ls-files",
	"git blame", "git stash list", "go version", "go env",
	"node -v", "npm -v", "npx", "python --version", "pip list",
	"cargo --version", "rustc --version",
}

func IsSafeCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	for _, prefix := range safeCommandPrefixes {
		if cmd == prefix || strings.HasPrefix(cmd, prefix+" ") || strings.HasPrefix(cmd, prefix+"\t") {
			if !strings.Contains(cmd, ">") && !strings.Contains(cmd, "|") &&
				!strings.Contains(cmd, ";") && !strings.Contains(cmd, "&&") &&
				!strings.Contains(cmd, "$(") && !strings.Contains(cmd, "`") {
				return true
			}
		}
	}
	return false
}

// Content extraction for rule matching

var contentFields = map[string]string{
	"Bash": "command", "ReadFile": "file_path", "WriteFile": "file_path",
	"EditFile": "file_path", "Glob": "pattern", "Grep": "pattern",
}

func ExtractContent(toolName string, args map[string]any) string {
	field, ok := contentFields[toolName]
	if !ok {
		return ""
	}
	v, _ := args[field].(string)
	return v
}

// DescribeToolAction 为 HITL 确认生成人类可读的操作描述。
// 优先从标准内容字段（command, file_path 等）提取；无法提取时拼接参数摘要。
func DescribeToolAction(toolName string, args map[string]any) string {
	content := ExtractContent(toolName, args)
	if content != "" {
		return content
	}
	// 无标准字段时，拼接参数的简短摘要
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	if len(parts) > 0 {
		return strings.Join(parts, ", ")
	}
	return toolName
}

// Layer 4+5: Permission Checker (orchestrates all layers)

type Checker struct {
	Sandbox      *PathSandbox
	RuleEngine   *RuleEngine
	Mode         PermissionMode
	PlanFilePath string
	// SandboxEnabled 表示是否启用 OS 级沙箱。
	// 启用后 Bash 命令在沙箱内执行，配合 autoAllow 可跳过确认。
	SandboxEnabled bool
	// sessionAllowed 是 Layer 4b：会话级 allow-always 集合（内存中）。
	// 存放格式为 "ToolName:pattern"，用户选择 always allow 时记录。
	// 会话结束即消失，不写入磁盘。
	sessionAllowed map[string]bool
	// Audit receives write-class decisions (阶段四 任务 4.2). Optional and, by contract,
	// non-blocking: the audit writer is a fire-and-forget sink, and a nil Audit disables auditing
	// entirely rather than failing the check.
	//
	// Declared as a narrow interface rather than the concrete *audit.Writer so this package does
	// not import internal/audit (which would be a needless coupling for one method).
	Audit AuditSink
	// SessionID / AgentName are stamped onto audit records so "who did this" is answerable.
	SessionID string
	AgentName string
}

// AuditSink is the write-class audit hook. Record must not block.
type AuditSink interface {
	Record(AuditEntry)
}

// AuditEntry is the permission package's own view of an auditable event.
//
// It exists so permissions never has to import internal/audit just to describe an event.
// internal/audit owns the storage format; this is the vocabulary, and the adapter that maps one
// to the other lives in the wiring layer (tui/remote).
type AuditEntry struct {
	Time      string
	SessionID string
	Agent     string
	Tool      string
	Target    string
	Action    string
	Decision  string
	Outcome   string
	Workspace string
	Reason    string
}

// EmitAudit records one entry when a sink is wired. Silently no-ops otherwise.
func (c *Checker) EmitAudit(e AuditEntry) {
	if c == nil || c.Audit == nil {
		return
	}
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	if e.SessionID == "" {
		e.SessionID = c.SessionID
	}
	if e.Agent == "" {
		e.Agent = c.AgentName
	}
	c.Audit.Record(e)
}

func NewChecker(sandbox *PathSandbox, ruleEngine *RuleEngine, mode PermissionMode) *Checker {
	return &Checker{
		Sandbox:        sandbox,
		RuleEngine:     ruleEngine,
		Mode:           mode,
		sessionAllowed: make(map[string]bool),
	}
}

// AddSessionAllow 将工具+内容模式加入会话级放行集合（Layer 4b）。
// 同时写入持久化规则引擎，以便下次会话也生效。
func (c *Checker) AddSessionAllow(toolName, content string) {
	key := toolName + ":" + content
	c.sessionAllowed[key] = true
}

// checkSessionAllowed 检查是否匹配会话级放行记录。
func (c *Checker) checkSessionAllowed(toolName, content string) bool {
	if len(c.sessionAllowed) == 0 {
		return false
	}
	key := toolName + ":" + content
	if c.sessionAllowed[key] {
		return true
	}
	// 前缀匹配：已记录的 pattern 可能带通配尾缀 *
	for allowed := range c.sessionAllowed {
		if strings.HasSuffix(allowed, "*") && strings.HasPrefix(key, allowed[:len(allowed)-1]) {
			return true
		}
	}
	return false
}

// Check is the audited entry point: it delegates to check and records write-class decisions.
//
// Wrapping rather than sprinkling audit calls at the ~10 return points inside check is deliberate.
// An audit hook added at each return is exactly the kind of thing that gets forgotten the next
// time someone adds a new branch — and a permission layer that silently stops recording one kind
// of decision is worse than no audit at all, because it looks complete. One wrapper cannot be
// bypassed by a new return statement.
//
// Only WRITE-class decisions are recorded. Reads produce a decision on nearly every tool call and
// would swamp the log with entries nobody reads; acceptance standard 4 asks for write traceability
// specifically.
func (c *Checker) Check(tool tools.Tool, args map[string]any) Decision {
	d := c.check(tool, args)

	if c.Audit != nil && tool.Category() == tools.CategoryWrite {
		outcome := ""
		switch d.Effect {
		case Allow:
			outcome = "auto-allowed"
		case Ask:
			if d.ReadonlyWrite {
				outcome = "pending-readonly-approval"
			} else {
				outcome = "pending-approval"
			}
		case Deny:
			if d.ReadonlyWrite {
				outcome = "denied-readonly"
			} else {
				outcome = "denied"
			}
		}
		c.EmitAudit(AuditEntry{
			Tool:      tool.Name(),
			Target:    ExtractContent(tool.Name(), args),
			Action:    SummarizeAction(tool.Name(), args),
			Decision:  string(d.Effect),
			Outcome:   outcome,
			Workspace: d.Workspace,
			Reason:    d.Reason,
		})
	}
	return d
}

// RecordApproval writes the final outcome of a human-in-the-loop prompt.
//
// Separate from Check because the answer arrives later and out of band: Check records that the
// prompt appeared ("pending-approval"), this records what the user actually chose. Both lines
// matter for a trail — "asked and was denied" and "asked and never answered" look identical
// otherwise.
func (c *Checker) RecordApproval(toolName, target, workspace string, approved bool, readonly bool) {
	if c.Audit == nil {
		return
	}
	outcome := "approved-by-user"
	if !approved {
		outcome = "denied-by-user"
		if readonly {
			outcome = "denied-readonly"
		}
	}
	c.EmitAudit(AuditEntry{
		Tool:      toolName,
		Target:    target,
		Action:    "user approval prompt",
		Decision:  "ask",
		Outcome:   outcome,
		Workspace: workspace,
	})
}

func (c *Checker) check(tool tools.Tool, args map[string]any) Decision {
	content := ExtractContent(tool.Name(), args)
	cat := tool.Category()

	// Layer 0: Plan mode plan-file write exception
	if c.Mode == ModePlan && cat == tools.CategoryWrite && isPlanFile(content, c.PlanFilePath) {
		return Decision{Effect: Allow, Reason: "Plan mode: plan file write allowed"}
	}

	// Layer 1: safe read-only commands (auto-allow)
	if cat == tools.CategoryCommand && IsSafeCommand(content) {
		return Decision{Effect: Allow, Reason: "Safe read-only command"}
	}

	// Layer 1b: 沙箱自动放行 — 命令在 OS 沙箱内执行时无需确认，
	// 但显式 deny/ask 规则仍然生效。对齐 Claude Code checkSandboxAutoAllow：
	// 拆分复合命令逐条检查，任何子命令触发 deny 则整体 deny，触发 ask 则弹窗。
	if c.SandboxEnabled && cat == tools.CategoryCommand {
		subcommands := splitCompoundCommand(content)
		var hasAsk bool
		for _, sub := range subcommands {
			r := c.RuleEngine.Evaluate(tool.Name(), sub)
			if r != nil && *r == RuleDeny {
				return Decision{Effect: Deny, Reason: "Permission rule: deny"}
			}
			if r != nil && *r == RuleAsk {
				hasAsk = true
			}
		}
		if hasAsk {
			return Decision{Effect: Ask, Reason: "Permission rule: ask (sandbox does not override explicit ask)"}
		}
		return Decision{Effect: Allow, Reason: "Sandboxed: auto-allow"}
	}

	// Layer 2: dangerous command (Bash only)
	if cat == tools.CategoryCommand {
		hit, reason := DetectDangerous(content)
		if hit {
			return Decision{Effect: Deny, Reason: fmt.Sprintf("Dangerous command blocked: %s", reason)}
		}
	}

	// Layer 3: path sandbox (file tools)
	//
	// 阶段四 4.2: reads and writes now go through DIFFERENT sandbox checks. A read landing in a
	// readonly workspace is fine; a WRITE there escalates to Ask with AllowAlways disabled
	// (ReadonlyWrite) rather than being silently denied or silently allowed.
	if (cat == tools.CategoryRead || cat == tools.CategoryWrite) && content != "" {
		if cat == tools.CategoryRead {
			if ok, reason := c.Sandbox.CheckRead(content); !ok && c.Mode != ModeBypass {
				return Decision{Effect: Ask, Reason: fmt.Sprintf("Path sandbox: %s", reason)}
			}
		} else {
			if ok, reason := c.Sandbox.CheckWrite(content); !ok {
				readonlyHit, wsName := c.Sandbox.ReadonlyViolation(content)
				switch {
				case c.Mode == ModeBypass:
					// bypass 模式跳过沙箱确认，直接放行（保持历史行为）
				case readonlyHit:
					return Decision{
						Effect: Ask,
						Reason: fmt.Sprintf("Read-only workspace %s: writing here requires explicit approval", wsName),
						// AllowAlways 禁用：只读语义不能被一次"always"永久打破。
						ReadonlyWrite: true,
						Workspace:     wsName,
					}
				default:
					return Decision{Effect: Ask, Reason: fmt.Sprintf("Path sandbox: %s", reason)}
				}
			}
		}
	}

	// Layer 4: rule engine
	ruleResult := c.RuleEngine.Evaluate(tool.Name(), content)
	if ruleResult != nil {
		if *ruleResult == RuleAllow {
			return Decision{Effect: Allow, Reason: "Permission rule: allow"}
		}
		return Decision{Effect: Deny, Reason: "Permission rule: deny"}
	}

	// Layer 4b: 会话级放行（内存中，优先于模式兜底）
	if c.checkSessionAllowed(tool.Name(), content) {
		return Decision{Effect: Allow, Reason: "Session allow-always"}
	}

	// Layer 4: permission mode
	effect := ModeDecide(c.Mode, cat)
	if effect == Allow {
		return Decision{Effect: Allow, Reason: fmt.Sprintf("Permission mode %s: allow", c.Mode)}
	}
	if effect == Deny {
		return Decision{Effect: Deny, Reason: fmt.Sprintf("Permission mode %s: deny", c.Mode)}
	}

	// Layer 5: ASK → HITL
	return Decision{Effect: Ask, Reason: "User confirmation required"}
}

func isPlanFile(targetPath, planPath string) bool {
	if planPath == "" || targetPath == "" {
		return false
	}
	// Try absolute path comparison
	absTarget, err1 := filepath.Abs(targetPath)
	absPlan, err2 := filepath.Abs(planPath)
	if err1 == nil && err2 == nil && absTarget == absPlan {
		return true
	}
	// Check if target ends with the plan file's relative suffix
	cleanTarget := filepath.Clean(targetPath)
	cleanPlan := filepath.Clean(planPath)
	if cleanTarget == cleanPlan {
		return true
	}
	// Base name match: LLM occasionally shortens file_path to just the base name.
	// The plan slug is randomly generated (adjective+noun+timestamp), so collision
	// with an unrelated file under the same name is extremely unlikely.
	if filepath.Base(cleanTarget) == filepath.Base(cleanPlan) {
		return true
	}
	return false
}

// SummarizeAction renders a one-line description of what a tool call was asked to do, for the
// audit trail's "action" field.
//
// An audit record that only says "WriteFile /repo/foo.go" leaves the reader guessing what was
// written. This adds the distinguishing argument (the first line of new content, the replaced
// string, the command) so the log is readable without cross-referencing the diff.
func SummarizeAction(toolName string, args map[string]any) string {
	str := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	switch toolName {
	case "WriteFile":
		if c := firstLine(str("content")); c != "" {
			return "write file, content starts: " + c
		}
		return "write file"
	case "EditFile":
		if old := firstLine(str("old_string")); old != "" {
			return "edit file, replacing: " + old
		}
		return "edit file"
	case "Bash":
		return firstLine(str("command"))
	case "NotebookEdit", "ApplyPatch":
		return toolName + " operation"
	default:
		if p := ExtractContent(toolName, args); p != "" {
			return toolName + " on " + p
		}
		return toolName + " operation"
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const max = 80
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// AddWritableRoot 注册一个可写工作区根路径（阶段四 任务 4.3）。
//
// 与 NewPathSandbox 的 extraAllowed 参数分开：那个是构造期的固定集合，
// 这个是运行期（/workspace add、子 agent 收窄沙箱）动态追加的。
func (s *PathSandbox) AddWritableRoot(path string) {
	abs, err := resolveSandboxPath(path)
	if err != nil {
		return
	}
	if s.explicitWritable == nil {
		s.explicitWritable = map[string]bool{}
	}
	for _, existing := range s.allowedRoots {
		if existing == abs {
			s.explicitWritable[abs] = true
			return
		}
	}
	s.allowedRoots = append(s.allowedRoots, abs)
	s.explicitWritable[abs] = true
}

// resolveSandboxPath normalizes a path for sandbox comparison: absolute + cleaned + symlinks
// resolved.
//
// 阶段四: the symlink step is load-bearing, not hygiene. On macOS /var is a symlink to
// /private/var, and t.TempDir() lives under it — so a readonly root registered as
// "/private/var/.../pluto_feed" would never match a target compared as "/var/.../pluto_feed",
// and the readonly boundary would silently not apply. internal/workspace already resolves on
// registration; this makes the sandbox side agree, so both sides of every comparison are
// resolved-to-resolved.
//
// EvalSymlinks failure (path doesn't exist yet — a file about to be created) falls back to the
// Abs+Clean result: refusing to resolve must not mean refusing to check.
func resolveSandboxPath(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}

	// The path does not exist yet — which is the NORMAL case for a file the agent is about to
	// create, and therefore the normal case for a write that read-only must block.
	//
	// Resolving only the parent directory and re-appending the base name is what makes the
	// comparison work at all: on macOS the parent resolves to /private/var/... while the
	// unresolved leaf would stay /var/..., and the two would never match. Falling back to the
	// fully unresolved path here silently disabled read-only enforcement for every new file.
	parent := filepath.Dir(abs)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(resolvedParent, filepath.Base(abs)), nil
	}
	return abs, nil
}
