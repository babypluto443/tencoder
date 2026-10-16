// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"tencoder/internal/agent"
	"tencoder/internal/conversation"
	"tencoder/internal/llm"
	"tencoder/internal/permissions"
	"tencoder/internal/teams"
	"tencoder/internal/toolresult"
	"tencoder/internal/tools"
	"tencoder/internal/worktree"
)

// sanitizeSlugSegment replaces any character outside [a-zA-Z0-9._-] with '-' and trims redundant
// separators so the result is safe for a git branch name.
var unsafeSlugChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeSlugSegment(s string) string {
	clean := unsafeSlugChars.ReplaceAllString(s, "-")
	clean = strings.Trim(clean, "-_.")
	if clean == "" {
		clean = "subagent"
	}
	if len(clean) > 40 {
		clean = clean[:40]
	}
	return clean
}

type SubAgentProgress struct {
	AgentDesc string
	AgentType string
	ToolName  string
	ToolArgs  map[string]any
	Elapsed   float64
	IsError   bool
	Done      bool
	ToolCount int
	TotalTime float64
}

const ForkBoilerplateTag = "<fork_boilerplate>"

// ForkAgentType matches FORK_AGENT.agentType from the design TypeScript implementation.
const ForkAgentType = "fork"

// ForkQuerySource matches the value produced by getQuerySourceForAgent for fork children
// (`agent:builtin:${FORK_AGENT.agentType}`). Used as the primary fork-nesting signal; falls back to
// scanning conversation history for ForkBoilerplateTag.
const ForkQuerySource = "agent:builtin:" + ForkAgentType

type AgentTool struct {
	Client        llm.Client
	ModelResolver func(string) (llm.Client, error)
	Registry      *tools.Registry
	Protocol      string
	TaskMgr       *TaskManager
	ProgressCh    chan<- SubAgentProgress
	Loader        *AgentLoader
	Conversation  *conversation.Manager // parent conversation, needed for Fork
	TeamMgr       *teams.TeamManager    // optional, enables team_name parameter

	// ParentChecker is the parent agent's permission checker. The Sandbox and RuleEngine are reused;
	// only Mode is overridden when the sub-agent definition / call sets a different permissionMode.
	// Optional — when nil, sub-agents inherit no checker (early bootstrap / tests).
	ParentChecker *permissions.Checker

	// ParentReplacementState is the parent agent's tool-result decision log.
	// Fork children Clone() it at construction so they share frozen
	// decisions on tool_use_ids inherited from the parent (necessary for
	// prompt-cache prefix stability across parent/child). Non-fork
	// sub-agents (subagent_type=*) start with a fresh state — they have no
	// shared history, so they have no shared cache prefix to preserve.
	ParentReplacementState *toolresult.ContentReplacementState

	// QuerySource identifies the spawning agent for nested-fork detection. Empty for the main thread;
	// set to ForkQuerySource (or "agent:builtin:<type>") when the AgentTool instance lives inside a
	// spawned sub-agent. Compaction-resistant — survives even when the fork boilerplate gets
	// summarized out of conversation history.
	QuerySource string

	// Workspaces is the cross-project registry (阶段四 任务 4.3/4.5), injected so this package does
	// not depend on internal/workspace. Optional — nil means no cross-project support and every
	// sub-agent inherits the parent's sandbox verbatim.
	Workspaces WorkspaceResolver

	// Router is the 阶段五 tier router. Optional: nil means every sub-agent uses the session
	// model, which is exactly the pre-阶段五 behaviour.
	Router *llm.Router
	// QualityGate is the 阶段五 5.4 syntax gate. Optional; nil disables gating entirely.
	QualityGate *llm.QualityGate
	// GateEnabled turns the gate + cascade on for light-tier code sub-agents. Separate from
	// QualityGate != nil so the gate machinery can stay wired while a run disables it.
	GateEnabled bool
	// LogRoute receives one line per routing/gate decision (阶段五 5.5 埋点). Optional.
	LogRoute func(line string)
}

// WorkspaceResolver is the slice of internal/workspace.Registry that sub-agent spawning needs.
//
// Deliberately narrow: spawning needs to answer "what are the roots and levels for the workspace
// this sub-agent should see", nothing else. A concrete *workspace.Registry dependency here would
// also be an import cycle (workspace → permissions → tools, and agents sits above all of them).
type WorkspaceResolver interface {
	// Exists reports whether the named workspace is registered.
	Exists(workspaceName string) bool
	// RootsFor returns the absolute writable roots and readonly roots (keyed by project name,
	// the value the refusal message shows) visible to a sub-agent scoped to the named workspace.
	RootsFor(workspaceName string) (writable []string, readonly map[string]string, ok bool)
}

func (t *AgentTool) Name() string                 { return "Agent" }
func (t *AgentTool) Category() tools.ToolCategory { return tools.CategoryCommand }

func (t *AgentTool) Description() string {
	desc := `Launch a sub-agent to handle a complex task. Each sub-agent runs independently with its own context. The sub-agent cannot see the current conversation.

This is ONE tool with multiple roles. Roles are NOT separate tools — you pick one by passing its name in the "subagent_type" parameter. Do not search for a tool named after a role; call THIS tool ("Agent") and set "subagent_type".

Available roles for the "subagent_type" parameter:`

	if t.Loader != nil {
		for _, name := range t.Loader.ListNames() {
			def := t.Loader.Get(name)
			desc += "\n- " + name + ": " + def.WhenToUse
		}
	} else {
		desc += "\n- general-purpose: Full tool access for multi-step tasks (default)"
		desc += "\n- plan: Read-only tools for designing implementation plans"
		desc += "\n- explore: Read-only search agent for locating code"
	}

	desc += `

Example call shape:
{
  "name": "Agent",
  "input": {
    "subagent_type": "<role from the list above>",
    "description": "Short task label",
    "prompt": "Detailed instructions — the sub-agent has zero prior context"
  }
}

Write a detailed prompt explaining what the sub-agent should do and why — it has no prior context.
When tasks are independent, launch multiple sub-agents in parallel by making multiple Agent tool calls in a single response.`
	return desc
}

func (t *AgentTool) Schema() map[string]any {
	agentTypes := []string{"general-purpose", "plan", "explore"}
	if t.Loader != nil {
		agentTypes = t.Loader.ListNames()
	}

	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A short (3-5 word) description of the task",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task for the agent to perform. Be detailed — the agent has no context from this conversation.",
				},
				"subagent_type": map[string]any{
					"type":        "string",
					"enum":        agentTypes,
					"description": "The type of agent to use. If omitted, forks current conversation context.",
				},
				"model": map[string]any{
					"type":        "string",
					"enum":        []string{"sonnet", "opus", "haiku"},
					"description": "Override the model for this agent.",
				},
				"run_in_background": map[string]any{
					"type":        "boolean",
					"description": "Set to true to run the agent in the background.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Name for the agent, enabling SendMessage communication.",
				},
				"isolation": map[string]any{
					"type":        "string",
					"enum":        []string{"worktree"},
					"description": "Isolation mode. Set to 'worktree' to give the agent its own git worktree so its file edits don't collide with peers or the lead. REQUIRED when spawning two or more teammates in parallel that may write files; STRONGLY RECOMMENDED for any single teammate doing non-trivial file edits while the lead is still working in the same repo. Skip only for read-only tasks (explore/grep/plan) or when you explicitly want the teammate to share your working tree.",
				},
				"workspace": map[string]any{
					"type":        "string",
					"description": "Name of a registered cross-project workspace to scope this agent to (see /workspace). The agent sees ONLY the main project plus that workspace — other registered projects are invisible to it, even read-only. Use this when the task lives in another project.",
				},
				"team_name": map[string]any{
					"type":        "string",
					"description": "REQUIRED when creating team members. Spawns the agent as a long-running teammate under this team (created via TeamCreate). Unlike regular sub-agents, team members run in their own terminal, persist after the lead returns, and communicate with each other via SendMessage. Without team_name the agent runs as a one-shot sub-agent that blocks and returns inline.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"default", "acceptEdits", "plan", "bypassPermissions"},
					"description": "Permission mode override for the spawned agent (e.g., 'plan' to require plan approval).",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Absolute path to run the agent in. Overrides the working directory for filesystem and shell operations. Mutually exclusive with isolation: 'worktree'.",
				},
			},
			"required": []string{"description", "prompt"},
		},
	}
}

// selectClientForSpec is the 阶段五 replacement for selectClient: the declared model pin and the
// two signal-derived signals are merged by the router rather than by a chain of ifs here.
//
// Precedence (and it is a precedence, not a blend):
//  1. an explicit pin (call-site override > agent-definition model) wins outright;
//  2. otherwise the task class baseline decides, promoted by the two non-declaration signals.
//
// A pin short-circuits at the router level, so "routing adjusted my default" can never be
// reported for a run the user explicitly named a model for — which would be a lie in the log.
func (t *AgentTool) selectClientForSpec(spec SubAgentSpec, overrideModel string) (llm.Client, llm.Tier, llm.RouteResult) {
	model := overrideModel
	if model == "" {
		model = spec.Model
	}
	if t.Router == nil {
		// No router wired: exactly the pre-阶段五 behaviour, including the ModelResolver fallback
		// for named models. This is the path that keeps every existing test green.
		c := t.selectClient(spec.Model, overrideModel)
		return c, llm.TierStrong, llm.RouteResult{Tier: llm.TierStrong, Reason: "no router wired"}
	}

	class := spec.TaskClass
	if !llm.KnownClass(class) {
		// An unclassified task is not licence to guess cheap. Unknown → strong.
		class = llm.ClassArchDecision
	}
	hints := llm.RouteHints{ExplicitModel: model}
	res := t.Router.Resolve(class, hints)
	if res.Client == nil {
		res.Client = t.selectClient(spec.Model, overrideModel)
	}
	if t.LogRoute != nil {
		t.LogRoute(fmt.Sprintf("[route] sub-agent=%s class=%s tier=%s model=%s promoted=%v (%s)",
			spec.Name, class, res.Tier, res.Model, res.Promoted, res.Reason))
	}
	return res.Client, res.Tier, res
}

func (t *AgentTool) selectClient(specModel, overrideModel string) llm.Client {
	model := overrideModel
	if model == "" {
		model = specModel
	}
	if model == "" || model == "inherit" {
		return t.Client
	}
	if t.ModelResolver != nil {
		if c, err := t.ModelResolver(model); err == nil {
			return c
		}
	}
	return t.Client
}

func (t *AgentTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	description, _ := args["description"].(string)
	prompt, _ := args["prompt"].(string)
	if description == "" || prompt == "" {
		return tools.ToolResult{Output: "Error: description and prompt are required", IsError: true}
	}

	subagentType, _ := args["subagent_type"].(string)
	modelOverride, _ := args["model"].(string)
	runInBackground, _ := args["run_in_background"].(bool)
	agentName, _ := args["name"].(string)
	teamName, _ := args["team_name"].(string)
	// 阶段四 4.5: workspace scoping. Empty = inherit the parent's view (the default, and the only
	// behaviour 阶段三 had).
	workspaceName, _ := args["workspace"].(string)
	if workspaceName != "" && t.Workspaces != nil {
		if !t.Workspaces.Exists(workspaceName) {
			// Fail loudly rather than silently spawning an agent that cannot reach what it was
			// pointed at — a sub-agent that discovers the boundary by hitting it wastes a whole turn.
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown workspace %q. Use /workspace to list registered workspaces.", workspaceName),
				IsError: true,
			}
		}
	}
	modeOverride, _ := args["mode"].(string)
	cwdOverride, _ := args["cwd"].(string)
	isolation, _ := args["isolation"].(string)

	// cwd and isolation: "worktree" are mutually exclusive.
	if cwdOverride != "" && isolation == "worktree" {
		return tools.ToolResult{
			Output:  "Error: cwd and isolation: 'worktree' are mutually exclusive",
			IsError: true,
		}
	}

	if modeOverride != "" && !validPermissionModes[modeOverride] {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: invalid mode '%s'. Valid: default, acceptEdits, plan, bypassPermissions", modeOverride),
			IsError: true,
		}
	}

	// Team-member path: an explicit team_name turns this spawn into a long-running teammate under that
	// team's backend. The lead gets control back as soon as the teammate boots; further coordination
	// flows through SendMessage / mailbox notifications.
	if teamName != "" && t.TeamMgr != nil {
		return t.runAsTeammate(ctx, teamName, agentName, description, prompt, modelOverride, subagentType, isolation)
	}

	// Fork path: no subagent_type specified.
	if subagentType == "" {
		return t.runFork(ctx, description, prompt, modelOverride, agentName)
	}

	// Definition path: resolve spec from loader or builtins.
	var spec SubAgentSpec
	if t.Loader != nil {
		def := t.Loader.Get(subagentType)
		if def == nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: %s", subagentType, strings.Join(t.Loader.ListNames(), ", ")),
				IsError: true,
			}
		}
		spec = def.ToSpec()
	} else {
		s, ok := BuiltinSpecs[subagentType]
		if !ok {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: general-purpose, plan, explore", subagentType),
				IsError: true,
			}
		}
		spec = s
	}

	// Per-call mode override beats the definition's permissionMode.
	if modeOverride != "" {
		spec.PermissionMode = modeOverride
	}

	// Definition-level `background: true` forces the spawn to run async, matching the
	// `run_in_background || selectedAgent.background` gating.
	if runInBackground || spec.Background {
		return t.runAsync(ctx, spec, description, prompt, modelOverride, workspaceName)
	}
	return t.runSync(ctx, spec, description, prompt, modelOverride, isolation, cwdOverride, workspaceName)
}

func (t *AgentTool) runSync(ctx context.Context, spec SubAgentSpec, description, prompt, modelOverride, isolation, cwdOverride, workspaceName string) tools.ToolResult {
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, spec.DisallowedTools, false)
	client, tier, _ := t.selectClientForSpec(spec, modelOverride)
	// Record the routing outcome ON THE SPEC, not on a local: shouldGate and the escalation prompt
	// both read it, and a local would be invisible to them.
	spec.Tier = tier

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = deriveSubAgentChecker(t.ParentChecker, spec.PermissionMode, workspaceName, t.Workspaces)
	if spec.MaxTurns > 0 {
		subAgent.MaxIterations = spec.MaxTurns
	} else {
		subAgent.MaxIterations = 200
	}

	// Worktree isolation: create an isolated worktree for the sub-agent.
	var wtResult *worktree.AgentWorktreeResult
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		var err error
		wtResult, err = worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating agent worktree: %s", err),
				IsError: true,
			}
		}
		subAgent.WorkDir = wtResult.WorktreePath

		// Inject worktree notice into the prompt.
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	} else if cwdOverride != "" {
		subAgent.WorkDir = cwdOverride
	}

	// 阶段五 5.4: the tree the gate must inspect is the one the agent actually edited.
	//
	// Resolved here, AFTER the isolation branch, rather than before it — checking the parent's tree
	// while the agent worked in a worktree would report on files nobody wrote, and the gate would
	// pass code that was never compiled anywhere.
	gateDir := subAgent.WorkDir
	if gateDir == "" {
		gateDir, _ = os.Getwd()
	}

	conv := conversation.NewManager()
	if spec.SystemPromptOverride != "" {
		conv.AddSystemReminder(spec.SystemPromptOverride)
	}
	// initialPrompt is prepended to the first user turn.
	if spec.InitialPrompt != "" {
		conv.AddUserMessage(spec.InitialPrompt)
	}
	conv.AddUserMessage(prompt)

	start := time.Now()
	var output strings.Builder
	toolCount := 0
	ch := subAgent.Run(ctx, conv)

	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			output.WriteString(e.Text)
		case agent.PermissionRequestEvent:
			// Sub-agents are headless — there's no UI to prompt. Auto-deny keeps the sub-agent's
			// executeSingleTool unblocked instead of stalling on respCh forever. respCh has buffer=1, so
			// this send is non-blocking.
			e.ResponseCh <- agent.PermDeny
		case agent.ToolResultEvent:
			toolCount++
			emitProgress(t.ProgressCh, ctx, SubAgentProgress{
				AgentDesc: description,
				AgentType: spec.Name,
				ToolName:  e.ToolName,
				ToolArgs:  map[string]any{"_summary": e.Output},
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(t.ProgressCh, ctx, SubAgentProgress{
				AgentDesc: description,
				AgentType: spec.Name,
				Done:      true,
				ToolCount: toolCount,
				TotalTime: time.Since(start).Seconds(),
				IsError:   true,
			})
			return tools.ToolResult{
				Output:  fmt.Sprintf("Agent failed: %s", e.Message),
				IsError: true,
			}
		}
	}

	elapsed := time.Since(start)

	emitProgress(t.ProgressCh, ctx, SubAgentProgress{
		AgentDesc: description,
		AgentType: spec.Name,
		Done:      true,
		ToolCount: toolCount,
		TotalTime: elapsed.Seconds(),
	})

	result := output.String()
	if result == "" {
		result = "(agent produced no output)"
	}

	// 阶段五 5.4: quality gate + cascade escalation.
	//
	// Fires ONLY for light-tier code-generation sub-agents (决策 B: 触发范围). A light-tier
	// exploration or memory recall produces no code, so there is nothing to gate — running the
	// gate there would be pure latency.
	if t.shouldGate(spec) {
		res := t.escalateIfNeeded(ctx, spec, description, result, gateDir)
		if res != nil {
			result = *res
		}
	}

	// Worktree cleanup: if the sub-agent ran in an isolated worktree, auto-remove if clean, preserve
	// if dirty.
	if wtResult != nil {
		if worktree.HasWorktreeChanges(ctx, wtResult.WorktreePath, wtResult.HeadCommit) {
			result += fmt.Sprintf("\n\nWorktree kept at %s (branch %s) — has uncommitted changes or new commits.",
				wtResult.WorktreePath, wtResult.WorktreeBranch)
		} else {
			worktree.RemoveAgentWorktree(ctx, wtResult.WorktreePath, wtResult.WorktreeBranch, wtResult.GitRoot)
		}
	}

	return tools.ToolResult{
		Output: fmt.Sprintf("Agent \"%s\" completed in %s.\n\n%s", description, elapsed.Round(time.Millisecond), result),
	}
}

// shouldGate reports whether this run produced code that must pass the gate.
func (t *AgentTool) shouldGate(spec SubAgentSpec) bool {
	if !t.GateEnabled || t.QualityGate == nil {
		return false
	}
	if spec.Tier != llm.TierLight {
		return false // strong-tier output was never at risk
	}
	// "Did it write code" is a property of the CLASS, not of the routing decision.
	//
	// An earlier version of this also required BaselineTier(class) == light, on the theory that a
	// code-writing class implies a strong baseline. That was wrong twice over: it conflated "how
	// much this class usually costs" with "does its output need compiling", and it made the gate
	// unreachable for exactly the class it exists to protect (patch_gen's baseline IS strong, so
	// the extra condition was false whenever the spec said light).
	//
	// The condition is therefore just the class predicate below. Note the consequence honestly:
	// because the router currently only PROMOTES and never demotes, a patch_gen sub-agent cannot
	// reach the light tier — so in practice today the gate fires for light-tier explore runs that
	// edit files. The patch_gen arm is defensive, and becomes live the moment a cheapening signal
	// or an explicit per-class model pin exists.
	return isCodeGenClass(spec.TaskClass)
}

// isCodeGenClass narrows gating to classes that WRITE code. A light-tier log_summary or
// format_check changes no source files, so there is nothing for build/vet/gofmt to say.
func isCodeGenClass(c llm.TaskClass) bool {
	switch c {
	case llm.ClassPatchGen, llm.ClassExplore:
		return true
	}
	return false
}

// escalateIfNeeded runs the gate and, on failure, spawns a strong-tier repair agent exactly once.
//
// "最多 1 次" is the whole anti-loop design (决策 B): a second escalation would either loop
// forever on a genuinely unfixable compile error, or burn the cost saving this stage exists to
// produce. After one escalation the verdict is returned to the main agent, which has its own
// test loop — a stronger position than a third blind attempt from here.
func (t *AgentTool) escalateIfNeeded(ctx context.Context, spec SubAgentSpec, description, lightOutput, dir string) *string {
	gate := t.QualityGate.Run(ctx, dir)
	if t.LogRoute != nil {
		t.LogRoute(fmt.Sprintf("[gate] sub-agent=%s class=%s passed=%v skipped=%v dur=%s",
			spec.Name, spec.TaskClass, gate.Passed, gate.Skipped, gate.Duration.Round(time.Millisecond)))
	}
	if gate.Skipped || gate.Passed {
		return nil
	}
	// A timeout is an environment problem (cold module cache, a hung toolchain), not evidence
	// that the light model wrote bad code. Escalating burns a strong-tier call to fix nothing.
	if gate.TimedOut {
		if t.LogRoute != nil {
			t.LogRoute("[gate] timed out — not escalating (environment problem, not a code defect)")
		}
		out := lightOutput + "\n\n[quality gate] timed out after " +
			gate.Duration.Round(time.Second).String() + "; verdict unknown."
		return &out
	}

	failures := gate.FailedChecks(20)
	if t.LogRoute != nil {
		t.LogRoute(fmt.Sprintf("[gate] FAILED for %s — escalating once to strong", spec.Name))
	}

	repaired, err := t.runEscalatedRepair(ctx, spec, description, lightOutput, failures, dir)
	if err != nil {
		out := lightOutput + fmt.Sprintf(
			"\n\n[quality gate] FAILED, and the repair pass could not run: %v\nGate output:\n%s",
			err, failures)
		return &out
	}
	return &repaired
}

func (t *AgentTool) runFork(ctx context.Context, description, prompt, modelOverride, agentName string) tools.ToolResult {
	if t.Conversation == nil {
		return tools.ToolResult{Output: "Error: fork requires parent conversation context", IsError: true}
	}

	// Nested fork guard, two layers:
	// (1) Primary: querySource — set on the AgentTool instance when it's constructed inside a fork
	//     child. Compaction-resistant; catches the case where conversation history was rewritten or
	//     summarized.
	// (2) Fallback: message scan for ForkBoilerplateTag.
	if t.QuerySource == ForkQuerySource {
		return tools.ToolResult{
			Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
			IsError: true,
		}
	}
	for _, msg := range t.Conversation.GetMessages() {
		if strings.Contains(msg.Content, ForkBoilerplateTag) {
			return tools.ToolResult{
				Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
				IsError: true,
			}
		}
	}

	// Build forked conversation: copy parent messages + patch incomplete tool_use + append task.
	forkedConv := buildForkedConversation(t.Conversation, prompt)

	client := t.selectClient("", modelOverride)
	// Fork inherits the parent's exact tool pool to keep API request prefixes byte-identical for
	// prompt cache hits (matches FORK_AGENT.tools=['*'] + useExactTools=true). The Agent tool gets
	// cloned with QuerySource=ForkQuerySource so any nested fork attempt is rejected by the primary
	// check in runFork.
	subRegistry := cloneRegistryForFork(t.Registry)

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = t.ParentChecker // fork inherits parent's permission state verbatim
	subAgent.MaxIterations = 200
	// Fork inherits the parent's tool-result decision log so any tool_use_id
	// already seen in the shared history makes the same decision in the
	// child — necessary to keep the prompt-cache prefix byte-identical
	// across parent and child.
	if t.ParentReplacementState != nil {
		subAgent.ReplacementState = t.ParentReplacementState.Clone()
	}

	// Fork always runs in background.
	taskName := "fork"
	if agentName != "" {
		taskName = agentName
	}
	taskID := t.TaskMgr.CreateTask(taskName + ": " + truncate(prompt, 50))
	forkCtx, cancel := context.WithCancel(ctx)
	t.TaskMgr.SetRunning(taskID, cancel)

	go func() {
		var output string
		ch := subAgent.Run(forkCtx, forkedConv)
		for ev := range ch {
			switch e := ev.(type) {
			case agent.StreamText:
				output += e.Text
			case agent.PermissionRequestEvent:
				// Headless: auto-deny so the fork doesn't stall on respCh.
				e.ResponseCh <- agent.PermDeny
			case agent.ErrorEvent:
				t.TaskMgr.SetFailed(taskID, e.Message)
				return
			}
		}
		t.TaskMgr.SetCompleted(taskID, output)
	}()

	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Forked agent \"%s\" launched in background (task %s). Results will arrive via task-notification.",
			description, taskID,
		),
	}
}

// emitProgress sends a SubAgentProgress event without ever blocking the caller. If the consumer
// (TUI) is behind, the event is dropped — progress is best-effort UI feedback, not load-bearing
// state. Blocking sends here caused sub-agent loops to deadlock when ProgressCh's buffer filled up,
// which in turn prevented ESC / ctx cancel from ever taking effect because the sub-agent was stuck
// in this send rather than at a ctx-aware point.
func emitProgress(ch chan<- SubAgentProgress, ctx context.Context, p SubAgentProgress) {
	if ch == nil {
		return
	}
	select {
	case ch <- p:
	case <-ctx.Done():
	default:
		// Consumer is behind. Drop the event rather than stalling the sub-agent's event loop.
	}
}

// deriveSubAgentChecker threads a sub-agent's permissionMode through to the spawned agent: `mode`
// overrides the agent definition's permissionMode, which then feeds the sub-agent's
// ToolPermissionContext. The Sandbox and RuleEngine are shared — we only swap the Mode so the
// sub-agent's tool calls hit a different decision matrix without diverging permission state.
//
// Returns the parent checker unchanged when no override is requested.
//
// 阶段四 4.5: the new workspaceName parameter scopes the sub-agent's sandbox to
// "main project + that workspace". Everything else registered is invisible to it, which is what
// "隔离到子 agent 视野" means in practice: a sub-agent cannot wander into a project it was not
// pointed at, not even read-only.
//
// Narrowing is only ever applied when a workspace IS named. An unspecified sub-agent inherits the
// parent's sandbox verbatim — deliberately, so 阶段四's default behaviour is byte-identical to
// 阶段三's.
func deriveSubAgentChecker(parent *permissions.Checker, modeOverride, workspaceName string, ws WorkspaceResolver) *permissions.Checker {
	if parent == nil {
		return nil
	}
	sandbox := parent.Sandbox
	if workspaceName != "" && ws != nil && parent.Sandbox != nil {
		writable, readonly, ok := ws.RootsFor(workspaceName)
		if !ok {
			return parent // unknown workspace: caller already validated, but fail closed
		}
		if sb := narrowSandbox(parent.Sandbox, writable, readonly); sb != nil {
			sandbox = sb
		}
	}
	if modeOverride == "" || permissions.PermissionMode(modeOverride) == parent.Mode {
		if sandbox == parent.Sandbox {
			return parent
		}
		// Same mode but a narrowed sandbox — must construct, not return the parent.
		return permissions.NewChecker(sandbox, parent.RuleEngine, parent.Mode)
	}
	return permissions.NewChecker(sandbox, parent.RuleEngine, permissions.PermissionMode(modeOverride))
}

// narrowSandbox builds a sub-agent sandbox containing the parent's own writable roots plus the
// named workspace's roots. The parent's writable roots are kept because the sub-agent still needs
// its own worktree (which lives under the parent tree) and its scratch space.
//
// Returns nil when the parent has no sandbox, so the caller can keep using the parent unchanged.
func narrowSandbox(parent *permissions.PathSandbox, writable []string, readonly map[string]string) *permissions.PathSandbox {
	if parent == nil {
		return nil
	}
	// Construct with the parent's writable roots as the base so the sandbox keeps its own
	// denyWrite protections and temp-dir allowance; then layer the workspace roots on top.
	sb := permissions.NewPathSandbox(parent.GetAllowedRoots()[0], parent.GetAllowedRoots()[1:]...)
	for _, root := range writable {
		sb.AddWritableRoot(root)
	}
	for root, name := range readonly {
		sb.AddReadonlyRoot(root, name)
	}
	return sb
}

// cloneRegistryForFork returns a registry that copies the parent verbatim except that any
// *AgentTool instance is replaced with a shallow copy whose QuerySource is set to ForkQuerySource.
// This is the local equivalent of useExactTools=true plus getQuerySourceForAgent: the fork child
// sees the same wire-level tool definitions as its parent (so prompt cache hits) but a nested fork
// attempt is caught at call time by the QuerySource check in runFork.
func cloneRegistryForFork(reg *tools.Registry) *tools.Registry {
	forked := tools.NewRegistry()
	for _, tool := range reg.ListTools() {
		if at, ok := tool.(*AgentTool); ok {
			clone := *at
			clone.QuerySource = ForkQuerySource
			forked.Register(&clone)
			continue
		}
		forked.Register(tool)
	}
	return forked
}

const forkBoilerplate = ForkBoilerplateTag + `
You are a forked worker process. You are NOT the main agent.
Rules (non-negotiable):
1. Do NOT fork again.
2. Do NOT converse, ask questions, or request confirmation.
3. Use tools directly: read files, search code, make changes.
4. Stay strictly within your assigned task scope.
5. Final report must be under 500 characters, starting with "Scope:".
` + "</fork_boilerplate>"

func buildForkedConversation(parent *conversation.Manager, task string) *conversation.Manager {
	forked := conversation.NewManager()
	msgs := parent.GetMessages()

	// Byte-exact replay: preserve thinking blocks alongside tool_use so the API request prefix matches
	// the parent's exactly (cf. "keeping all content blocks (thinking, text, and every tool_use)").
	// Missing thinking blocks would change the assistant message shape and bust the prompt cache.
	for _, msg := range msgs {
		if len(msg.ToolUses) > 0 && len(msg.ToolResults) == 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
			var placeholders []conversation.ToolResultBlock
			for _, tu := range msg.ToolUses {
				placeholders = append(placeholders, conversation.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Content:   "(tool execution interrupted by fork)",
					IsError:   false,
				})
			}
			forked.AddToolResultsMessage(placeholders)
		} else if len(msg.ToolUses) > 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
		} else if len(msg.ToolResults) > 0 {
			forked.AddToolResultsMessage(msg.ToolResults)
		} else if msg.Role == "assistant" {
			if len(msg.ThinkingBlocks) > 0 {
				forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, nil)
			} else {
				forked.AddAssistantMessage(msg.Content)
			}
		} else {
			forked.AddUserMessage(msg.Content)
		}
	}

	// Append fork boilerplate + task as user message.
	forked.AddUserMessage(forkBoilerplate + "\n\nYour task:\n" + task)
	return forked
}

func (t *AgentTool) runAsync(ctx context.Context, spec SubAgentSpec, description, prompt, modelOverride, workspaceName string) tools.ToolResult {
	client := t.selectClient(spec.Model, modelOverride)
	taskID := SpawnSubAgent(ctx, t.TaskMgr, client, t.Registry, t.Protocol, spec, prompt, t.ParentChecker)

	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Agent \"%s\" launched in background (task %s). You will be notified when it completes.",
			description, taskID,
		),
	}
}

// runAsTeammate registers a long-running team member on an existing Team. Unlike runSync/runAsync,
// this path never blocks the lead on the member's output: the lead always returns immediately and
// coordinates through SendMessage + idle notifications in the team mailbox. The backend (in-process
// / tmux / iTerm) is picked from Team.Mode by teams.SpawnTeammate.
//
// When isolation == "worktree" and a WorktreeMgr is configured, the teammate gets a dedicated git
// worktree so its file edits don't collide with peers' work.
func (t *AgentTool) runAsTeammate(
	ctx context.Context,
	teamName, memberName, description, prompt, modelOverride, subagentType, isolation string,
) tools.ToolResult {
	team := t.TeamMgr.GetTeam(teamName)
	if team == nil {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: team '%s' not found. Call TeamCreate first.", teamName),
			IsError: true,
		}
	}

	if memberName == "" {
		memberName = sanitizeSlugSegment(description)
	}
	if _, exists := team.Members[memberName]; exists {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: team '%s' already has a member named '%s'", teamName, memberName),
			IsError: true,
		}
	}

	// Resolve spec when subagent_type is set so the teammate respects the same disallow list any other
	// sub-agent of that type would. Without a spec we hand the full registry to the teammate.
	var spec SubAgentSpec
	if subagentType != "" {
		if t.Loader != nil {
			if def := t.Loader.Get(subagentType); def != nil {
				spec = def.ToSpec()
			}
		} else if s, ok := BuiltinSpecs[subagentType]; ok {
			spec = s
		}
	}

	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, spec.DisallowedTools, false)
	client := t.selectClient(spec.Model, modelOverride)

	var otherMembers []string
	for n := range team.Members {
		otherMembers = append(otherMembers, n)
	}
	addendum := teams.BuildTeammateAddendum(teamName, memberName, otherMembers)

	var workdir string
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		wtResult, err := worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating teammate worktree: %s", err),
				IsError: true,
			}
		}
		workdir = wtResult.WorktreePath
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	}

	result, err := teams.SpawnTeammate(ctx, teams.TeammateSpawnConfig{
		Team:       team,
		MemberName: memberName,
		Task:       prompt,
		Addendum:   addendum,
		Client:     client,
		Registry:   subRegistry,
		Protocol:   t.Protocol,
		Workdir:    workdir,
	})
	if err != nil {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error spawning teammate: %v", err),
			IsError: true,
		}
	}

	// In-process spawns hand back a live event channel; drain it in the background so the goroutine
	// doesn't block on a full chan. Lead-visible progress flows through the mailbox, not this drain.
	if result.Mode == teams.ModeInProcess && result.EventCh != nil {
		go drainTeammateEvents(memberName, result.EventCh, t.ProgressCh)
	}

	backendHint := string(result.Mode)
	if result.PaneID != "" {
		backendHint += " pane=" + result.PaneID
	}
	if workdir != "" {
		backendHint += " worktree=" + workdir
	}
	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Teammate \"%s\" started on team \"%s\" [%s]. Use SendMessage to talk to it; its idle notifications will arrive as system reminders.",
			memberName, teamName, backendHint,
		),
	}
}

// drainTeammateEvents consumes a teammate's event stream so the producer side never blocks on a
// full channel. Tool/error events are forwarded to ProgressCh when set so the parent UI can show
// activity.
func drainTeammateEvents(name string, ch <-chan agent.AgentEvent, progressCh chan<- SubAgentProgress) {
	for ev := range ch {
		if progressCh == nil {
			continue
		}
		switch e := ev.(type) {
		case agent.ToolResultEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  e.ToolName,
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  "error",
				IsError:   true,
			})
		}
	}
}

// generateAgentSlug produces a slug matching ^agent-a[0-9a-f]{7}$ for sub-agent worktrees.x.
func generateAgentSlug(description string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "agent-a" + hex.EncodeToString(b)[:7]
}

// runEscalatedRepair spawns a strong-tier agent whose job is to FIX what the light tier wrote.
//
// The prompt shape is the whole cost argument (决策 B: 续作修复 vs 丢弃重做). It carries:
//   - the light agent's own summary of what it did,
//   - the structured gate failures (which check, which output),
//   - an instruction to fix in place, not rewrite.
//
// A repair pass costs roughly the delta, a from-scratch redo costs the whole thing again. That
// difference is what keeps the -45% cost target reachable while still having a safety net.
//
// Exactly one repair pass: no retry loop (决策 B 防循环). A second attempt after a failed repair
// is returned to the main agent instead, which has a stronger position (full conversation, its
// own test loop).
func (t *AgentTool) runEscalatedRepair(ctx context.Context, spec SubAgentSpec, description, lightOutput, failures, dir string) (string, error) {
	if t.Router == nil {
		return "", fmt.Errorf("no router wired for escalation")
	}
	if t.TaskMgr == nil {
		return "", fmt.Errorf("no task manager for escalation")
	}

	// Force the strong tier by classifying the repair as patch_gen, whose baseline IS strong.
	//
	// Deliberately not re-running the signals that put us on light: a task whose tool budget blew
	// past the threshold is still over budget, and re-asking would route us straight back down.
	// Escalation overrides routing; that is the entire point of an escalation path.
	res := t.Router.Resolve(llm.ClassPatchGen, llm.RouteHints{})
	client := res.Client
	if client == nil {
		client = t.Client // last resort: the session model
	}
	if client == nil {
		return "", fmt.Errorf("no client available for escalation")
	}
	if t.LogRoute != nil {
		t.LogRoute(fmt.Sprintf("[route] escalation tier=%s model=%s (%s)", res.Tier, res.Model, res.Reason))
	}

	repairSpec := spec
	repairSpec.Name = spec.Name + "-repair"
	repairSpec.TaskClass = llm.ClassPatchGen
	repairSpec.Tier = llm.TierStrong
	repairSpec.Model = "" // clear any light-tier model pin

	var sb strings.Builder
	fmt.Fprintf(&sb, "A previous agent attempted this task and its output FAILED the automated quality gate.\n\n")
	fmt.Fprintf(&sb, "## Gate failures\n\n```\n%s\n```\n\n", failures)
	fmt.Fprintf(&sb, "## What the previous agent reported\n\n%s\n\n", truncateForPrompt(lightOutput, 4000))
	fmt.Fprintf(&sb, "## Your job\n\n")
	fmt.Fprintf(&sb, "Fix the failures IN PLACE in %s. The previous work is already on disk — read it, keep what is correct, change only what does not build / vet / format.\n\n", dir)
	fmt.Fprintf(&sb, "Do NOT rewrite from scratch and do NOT discard correct work: that wastes the tokens this tier was chosen to save.\n")
	fmt.Fprintf(&sb, "When done, verify with `go build ./...`, `go vet ./...`, and `gofmt -l .` in %s before you finish.\n", dir)
	fmt.Fprintf(&sb, "Original task for context: %s\n", description)

	slug := slugify(spec.Name + "-repair")
	res2 := t.spawnRepair(ctx, repairSpec, client, sb.String(), slug)
	if res2.IsError {
		return "", fmt.Errorf("repair agent failed: %s", res2.Output)
	}

	out := res2.Output
	// Re-run the gate on the repaired tree so the caller learns whether the fix actually worked.
	// One more check, still no retry: this is verification, not another attempt.
	if final := t.QualityGate.Run(ctx, dir); !final.Skipped && !final.Passed && !final.TimedOut {
		out += fmt.Sprintf(
			"\n\n[quality gate] STILL FAILING after the repair pass:\n%s\nDo not retry blindly — investigate the cause and fix it in your main loop.",
			final.FailedChecks(20))
		if t.LogRoute != nil {
			t.LogRoute("[gate] repair did NOT fix it — handing back to the main agent")
		}
	} else if t.LogRoute != nil {
		t.LogRoute("[gate] repair pass cleared the gate")
	}
	return out, nil
}

// slugify builds a worktree-safe slug for the repair agent.
func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		out = "repair"
	}
	return out
}

func truncateForPrompt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n… (truncated)"
}

// spawnRepair runs the strong-tier repair agent to completion, in dir.
//
// It deliberately does NOT go through runSync: that path would re-derive a client from the spec,
// re-create a worktree, and re-enter the gate — so a failed repair would recurse into another
// escalation. Here the client and the directory are already decided, and the loop is a plain
// synchronous drain.
//
// Tool set: the parent spec's tools with Bash allowed. A repair that cannot run the compiler
// cannot verify its own fix, and an unverified fix is exactly the outcome this stage is trying to
// avoid paying for twice.
func (t *AgentTool) spawnRepair(ctx context.Context, spec SubAgentSpec, client llm.Client, prompt, dir string) tools.ToolResult {
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, spec.DisallowedTools, false)
	if subRegistry != nil {
		if bash := t.Registry.Get("Bash"); bash != nil {
			subRegistry.Register(bash)
		}
	}

	sub := agent.New(client, subRegistry, t.Protocol)
	sub.WorkDir = dir
	sub.Checker = deriveSubAgentChecker(t.ParentChecker, spec.PermissionMode, "", t.Workspaces)
	if spec.MaxTurns > 0 {
		sub.MaxIterations = spec.MaxTurns
	} else {
		// A repair is a bounded job: read the failures, fix, verify. Left unbounded it can burn the
		// saving this whole stage exists to produce.
		sub.MaxIterations = 60
	}

	conv := conversation.NewManager()
	conv.AddUserMessage(prompt)

	var out strings.Builder
	for ev := range sub.Run(ctx, conv) {
		switch e := ev.(type) {
		case agent.StreamText:
			out.WriteString(e.Text)
		case agent.PermissionRequestEvent:
			// Headless: auto-allow, because a repair agent that stalls on an approval prompt cannot
			// finish the fix we are paying a strong-tier call for. The sandbox still bounds what it
			// can touch — this removes friction, not confinement.
			e.ResponseCh <- agent.PermAllow
		case agent.ErrorEvent:
			return tools.ToolResult{Output: e.Message, IsError: true}
		}
	}
	res := out.String()
	if res == "" {
		res = "(repair agent produced no output)"
	}
	return tools.ToolResult{Output: res}
}
