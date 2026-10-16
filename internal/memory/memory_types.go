// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com


package memory

// Memory type taxonomy.
//
// Memories are constrained to four types capturing context NOT derivable
// from the current project state. Code patterns, architecture, git history,
// and file structure are derivable (via grep/git/TENCODER.md) and should NOT
// be saved as memories.
//
// A fifth type — TypeTask — exists but is NOT part of the model-facing
// taxonomy. Task memories are written by the system (the sub-task completion
// hook in task_write.go), never by the model itself: they are short-lived
// staging records under memory/tasks/ that get consolidated into long-term
// project memories when the session goes idle. See task_write.go /
// task_consolidate.go for the lifecycle.
type MemoryType string

const (
	TypeUser      MemoryType = "user"
	TypeFeedback  MemoryType = "feedback"
	TypeProject   MemoryType = "project"
	TypeReference MemoryType = "reference"

	// TypeTask is the short-lived episodic record of one finished sub-task:
	// goal → turning points → pitfalls + recovery path → result. Written by the
	// sub-task completion hook, distilled into project-level memories on idle,
	// then deleted. It parses as a valid type so frontmatter round-trips, but it
	// is deliberately absent from MemoryTypes so the model-facing taxonomy and
	// the "what not to save" rules keep describing exactly four writeable types.
	TypeTask MemoryType = "task"
)

// parseableTypes is the frontmatter type vocabulary: everything ParseMemoryType
// accepts. MemoryTypes is the subset the model is allowed to write.
var parseableTypes = []MemoryType{TypeUser, TypeFeedback, TypeProject, TypeReference, TypeTask}

// MemoryTypes is the model-facing writeable taxonomy (four types). TypeTask is
// excluded on purpose — see the type comment above.
var MemoryTypes = []MemoryType{TypeUser, TypeFeedback, TypeProject, TypeReference}

// ParseMemoryType validates a raw frontmatter value into a MemoryType.
// Returns ("", false) for invalid or missing values — legacy files without
// a `type:` field keep working, files with unknown types degrade gracefully.
// Accepts the four model-writable types plus the system-written `task`.
func ParseMemoryType(raw string) (MemoryType, bool) {
	for _, t := range parseableTypes {
		if string(t) == raw {
			return t, true
		}
	}
	return "", false
}

// TypesSectionDualPath is the `## Types of memory` section for DUAL-PATH mode:
// `user` and `feedback` memories live in the user-level dir (~/.tencoder/memory/)
// because they follow the human across projects; `project` and `reference`
// memories live in the project-level dir (./.tencoder/memory/) because they
// describe the current repo and should be team-shareable / git-trackable.
const TypesSectionDualPath = `## Types of memory

There are four discrete types of memory that you write yourself, plus a fifth (type: task) that Tencoder writes on your behalf. **The type decides which directory the file lives in** — user-level memories cross projects (so they're saved to the user-level dir), project-level memories belong to the current repo (so they're saved alongside the code).

<types>
<type>
    <name>user</name>
    <scope>user-level (~/.tencoder/memory/) — follows you across all projects</scope>
    <description>Contain information about the user's role, goals, responsibilities, and knowledge. Great user memories help you tailor your future behavior to the user's preferences and perspective. Your goal in reading and writing these memories is to build up an understanding of who the user is and how you can be most helpful to them specifically. Keep in mind, the aim here is to be helpful to the user. Avoid writing memories about the user that could be viewed as a negative judgement or that are not relevant to the work you're trying to accomplish together.</description>
    <when_to_save>When you learn any details about the user's role, preferences, responsibilities, or knowledge</when_to_save>
    <how_to_use>When your work should be informed by the user's profile or perspective. For example, if the user is asking you to explain a part of the code, you should answer that question in a way that is tailored to the specific details that they will find most valuable.</how_to_use>
    <examples>
    user: I'm a data scientist investigating what logging we have in place
    assistant: [saves user memory to ~/.tencoder/memory/: user is a data scientist, currently focused on observability/logging]

    user: I've been writing Go for ten years but this is my first time touching the React side of this repo
    assistant: [saves user memory to ~/.tencoder/memory/: deep Go expertise, new to React]
    </examples>
</type>
<type>
    <name>feedback</name>
    <scope>user-level (~/.tencoder/memory/) — feedback about how the human likes to work follows them</scope>
    <description>Guidance the user has given you about how to approach work — both what to avoid and what to keep doing. Record from failure AND success: if you only save corrections, you will avoid past mistakes but drift away from approaches the user has already validated.</description>
    <when_to_save>Any time the user corrects your approach ("no not that", "don't", "stop doing X") OR confirms a non-obvious approach worked ("yes exactly", "perfect, keep doing that"). Corrections are easy to notice; confirmations are quieter — watch for them.</when_to_save>
    <how_to_use>Let these memories guide your behavior so that the user does not need to offer the same guidance twice.</how_to_use>
    <body_structure>Lead with the rule itself, then a **Why:** line (the reason the user gave) and a **How to apply:** line (when/where this guidance kicks in).</body_structure>
    <examples>
    user: don't mock the database in these tests — we got burned last quarter when mocked tests passed but the prod migration failed
    assistant: [saves feedback memory to ~/.tencoder/memory/: integration tests must hit a real database, not mocks. Reason: prior incident with broken migration]

    user: stop summarizing what you just did at the end of every response, I can read the diff
    assistant: [saves feedback memory to ~/.tencoder/memory/: this user wants terse responses with no trailing summaries]
    </examples>
</type>
<type>
    <name>project</name>
    <scope>project-level (./.tencoder/memory/) — describes the current repo, can be committed for team use</scope>
    <description>Information that you learn about ongoing work, goals, initiatives, bugs, or incidents within the project that is not otherwise derivable from the code or git history. Project memories help you understand the broader context and motivation behind the work the user is doing within this working directory.</description>
    <when_to_save>When you learn who is doing what, why, or by when. These states change relatively quickly so try to keep your understanding of this up to date. Always convert relative dates to absolute dates when saving.</when_to_save>
    <how_to_use>Use these memories to more fully understand the details and nuance behind the user's request and make better informed suggestions.</how_to_use>
    <body_structure>Lead with the fact or decision, then a **Why:** line and a **How to apply:** line. Project memories decay fast, so the why helps future-you judge whether the memory is still load-bearing.</body_structure>
    <examples>
    user: we're freezing all non-critical merges after Thursday — mobile team is cutting a release branch
    assistant: [saves project memory to ./.tencoder/memory/: merge freeze begins 2026-03-05 for mobile release cut]

    user: the reason we're ripping out the old auth middleware is that legal flagged it for storing session tokens in a way that doesn't meet the new compliance requirements
    assistant: [saves project memory to ./.tencoder/memory/: auth middleware rewrite driven by legal/compliance, not tech debt]
    </examples>
</type>
<type>
    <name>reference</name>
    <scope>project-level (./.tencoder/memory/) — points to project-specific external resources</scope>
    <description>Stores pointers to where information can be found in external systems related to this project. These memories let you remember where to look for up-to-date information outside the project directory.</description>
    <when_to_save>When you learn about resources in external systems and their purpose. For example, that bugs are tracked in a specific project in Linear or that feedback can be found in a specific Slack channel.</when_to_save>
    <how_to_use>When the user references an external system or information that may be in an external system.</how_to_use>
    <examples>
    user: check the Linear project "INGEST" if you want context on these tickets, that's where we track all pipeline bugs
    assistant: [saves reference memory to ./.tencoder/memory/: pipeline bugs are tracked in Linear project "INGEST"]

    user: the Grafana board at grafana.internal/d/api-latency is what oncall watches — if you're touching request handling, that's the thing that'll page someone
    assistant: [saves reference memory to ./.tencoder/memory/: grafana.internal/d/api-latency is the oncall latency dashboard]
    </examples>
</type>
<type>
    <name>task</name>
    <scope>project-level, staging dir (./.tencoder/memory/tasks/) — written by Tencoder itself, not by you</scope>
    <description>Episodic record of one finished sub-task: the goal, the turning points that changed the outcome, the pitfalls hit and how each was recovered from, and the result. Transient by design — Tencoder writes these when a sub-agent finishes, surfaces them on recall, then consolidates them into project memories and deletes them. You never write or edit files under tasks/ yourself.</description>
    <when_to_save>Never directly. Tencoder's sub-task completion hook writes these on its own; your job is to surface them on recall and let idle consolidation fold the durable parts into project memories.</when_to_save>
    <how_to_use>Treat a recalled task memory as proof that a failure was already paid for once. If the current work touches the same area, follow the recorded recovery path instead of rediscovering the failure from scratch.</how_to_use>
    <examples>
    (system-written — never something you author)
    </examples>
</type>
</types>
`

// TypesSectionIndividual is the `## Types of memory` section for
// INDIVIDUAL-ONLY mode (single directory). No <scope> tags.
const TypesSectionIndividual = `## Types of memory

There are several discrete types of memory that you can store in your memory system, plus one (type: task) that Tencoder writes on its behalf when a sub-task finishes:

<types>
<type>
    <name>user</name>
    <description>Contain information about the user's role, goals, responsibilities, and knowledge. Great user memories help you tailor your future behavior to the user's preferences and perspective. Your goal in reading and writing these memories is to build up an understanding of who the user is and how you can be most helpful to them specifically. For example, you should collaborate with a senior software engineer differently than a student who is coding for the very first time. Keep in mind, that the aim here is to be helpful to the user. Avoid writing memories about the user that could be viewed as a negative judgement or that are not relevant to the work you're trying to accomplish together.</description>
    <when_to_save>When you learn any details about the user's role, preferences, responsibilities, or knowledge</when_to_save>
    <how_to_use>When your work should be informed by the user's profile or perspective. For example, if the user is asking you to explain a part of the code, you should answer that question in a way that is tailored to the specific details that they will find most valuable or that helps them build their mental model in relation to domain knowledge they already have.</how_to_use>
    <examples>
    user: I'm a data scientist investigating what logging we have in place
    assistant: [saves user memory: user is a data scientist, currently focused on observability/logging]

    user: I've been writing Go for ten years but this is my first time touching the React side of this repo
    assistant: [saves user memory: deep Go expertise, new to React and this project's frontend — frame frontend explanations in terms of backend analogues]
    </examples>
</type>
<type>
    <name>feedback</name>
    <description>Guidance the user has given you about how to approach work — both what to avoid and what to keep doing. These are a very important type of memory to read and write as they allow you to remain coherent and responsive to the way you should approach work in the project. Record from failure AND success: if you only save corrections, you will avoid past mistakes but drift away from approaches the user has already validated, and may grow overly cautious.</description>
    <when_to_save>Any time the user corrects your approach ("no not that", "don't", "stop doing X") OR confirms a non-obvious approach worked ("yes exactly", "perfect, keep doing that", accepting an unusual choice without pushback). Corrections are easy to notice; confirmations are quieter — watch for them. In both cases, save what is applicable to future conversations, especially if surprising or not obvious from the code. Include *why* so you can judge edge cases later.</when_to_save>
    <how_to_use>Let these memories guide your behavior so that the user does not need to offer the same guidance twice.</how_to_use>
    <body_structure>Lead with the rule itself, then a **Why:** line (the reason the user gave — often a past incident or strong preference) and a **How to apply:** line (when/where this guidance kicks in). Knowing *why* lets you judge edge cases instead of blindly following the rule.</body_structure>
    <examples>
    user: don't mock the database in these tests — we got burned last quarter when mocked tests passed but the prod migration failed
    assistant: [saves feedback memory: integration tests must hit a real database, not mocks. Reason: prior incident where mock/prod divergence masked a broken migration]

    user: stop summarizing what you just did at the end of every response, I can read the diff
    assistant: [saves feedback memory: this user wants terse responses with no trailing summaries]

    user: yeah the single bundled PR was the right call here, splitting this one would've just been churn
    assistant: [saves feedback memory: for refactors in this area, user prefers one bundled PR over many small ones. Confirmed after I chose this approach — a validated judgment call, not a correction]
    </examples>
</type>
<type>
    <name>project</name>
    <description>Information that you learn about ongoing work, goals, initiatives, bugs, or incidents within the project that is not otherwise derivable from the code or git history. Project memories help you understand the broader context and motivation behind the work the user is doing within this working directory.</description>
    <when_to_save>When you learn who is doing what, why, or by when. These states change relatively quickly so try to keep your understanding of this up to date. Always convert relative dates in user messages to absolute dates when saving (e.g., "Thursday" → "2026-03-05"), so the memory remains interpretable after time passes.</when_to_save>
    <how_to_use>Use these memories to more fully understand the details and nuance behind the user's request and make better informed suggestions.</how_to_use>
    <body_structure>Lead with the fact or decision, then a **Why:** line (the motivation — often a constraint, deadline, or stakeholder ask) and a **How to apply:** line (how this should shape your suggestions). Project memories decay fast, so the why helps future-you judge whether the memory is still load-bearing.</body_structure>
    <examples>
    user: we're freezing all non-critical merges after Thursday — mobile team is cutting a release branch
    assistant: [saves project memory: merge freeze begins 2026-03-05 for mobile release cut. Flag any non-critical PR work scheduled after that date]

    user: the reason we're ripping out the old auth middleware is that legal flagged it for storing session tokens in a way that doesn't meet the new compliance requirements
    assistant: [saves project memory: auth middleware rewrite is driven by legal/compliance requirements around session token storage, not tech-debt cleanup — scope decisions should favor compliance over ergonomics]
    </examples>
</type>
<type>
    <name>reference</name>
    <description>Stores pointers to where information can be found in external systems. These memories allow you to remember where to look to find up-to-date information outside of the project directory.</description>
    <when_to_save>When you learn about resources in external systems and their purpose. For example, that bugs are tracked in a specific project in Linear or that feedback can be found in a specific Slack channel.</when_to_save>
    <how_to_use>When the user references an external system or information that may be in an external system.</how_to_use>
    <examples>
    user: check the Linear project "INGEST" if you want context on these tickets, that's where we track all pipeline bugs
    assistant: [saves reference memory: pipeline bugs are tracked in Linear project "INGEST"]

    user: the Grafana board at grafana.internal/d/api-latency is what oncall watches — if you're touching request handling, that's the thing that'll page someone
    assistant: [saves reference memory: grafana.internal/d/api-latency is the oncall latency dashboard — check it when editing request-path code]
    </examples>
</type>
<type>
    <name>task</name>
    <description>Episodic record of one finished sub-task: the goal, the turning points that changed the outcome, the pitfalls hit and how each was recovered from, and the result. Transient by design — Tencoder writes these when a sub-agent finishes, surfaces them on recall, then consolidates them into project memories and deletes them. You never write or edit files under tasks/ yourself.</description>
    <when_to_save>Never directly. Tencoder's sub-task completion hook writes these on its own; your job is to surface them on recall and let idle consolidation fold the durable parts into project memories.</when_to_save>
    <how_to_use>Treat a recalled task memory as proof that a failure was already paid for once. If the current work touches the same area, follow the recorded recovery path instead of rediscovering the failure from scratch.</how_to_use>
    <examples>
    (system-written — never something you author)
    </examples>
</type>
</types>
`

// WhatNotToSaveSection is the `## What NOT to save in memory` section.
//
// Deliberately split in two (阶段一决策 B):
//   - WhatNotToSaveSection: the original rules, still governing the four
//     model-writable types (user / feedback / project / reference).
//   - WhatNotToSaveTaskSection: a whitelist for task memories. Task memories
//     are written by the sub-task completion hook, not by the model, so a
//     blacklist is the wrong shape — the hook needs a positive list of the
//     four things worth keeping, plus an explicit list of the derivable noise
//     it must refuse to transcribe.
const WhatNotToSaveSection = `## What NOT to save in memory

These rules apply to the four types you write yourself (user, feedback, project, reference):

- Code patterns, conventions, architecture, file paths, or project structure — these can be derived by reading the current project state.
- Git history, recent changes, or who-changed-what — ` + "`git log`" + ` / ` + "`git blame`" + ` are authoritative.
- Debugging solutions or fix recipes — the fix is in the code; the commit message has the context.
- Anything already documented in TENCODER.md files.
- Ephemeral task details: in-progress work, temporary state, current conversation context.

These exclusions apply even when the user explicitly asks you to save. If they ask you to save a PR list or activity summary, ask what was *surprising* or *non-obvious* about it — that is the part worth keeping.`

// WhatNotToSaveTaskSection constrains what goes into a task memory. It is
// consumed by the extraction prompt in task_write.go, not by the main agent's
// system prompt — see TaskExtractionSystemPrompt for where it actually lands.
const WhatNotToSaveTaskSection = `## What a task memory may contain (and what it must never contain)

A task memory records what was learned *during* one sub-task. It is a whitelist, not a blacklist —
if a piece of information is not on this list, it does not go in, even if it seems important.

Keep ONLY these four things:

1. **The pitfall and its cause** — not just the symptom. "The test failed because the fixture
   hardcoded an absolute path, not because the assertion was wrong" — the cause is the memory.
2. **The recovery path** — the action sequence that got unstuck: "check X before trying Y again",
   "run the package test before the full suite". A recovery path is only worth saving if following it
   would have saved real time.
3. **Constraints the user stated verbally during the task that cannot be found in the code** — e.g.
   "never touch the vendored dir, upstream patches land there weekly".
4. **The result's impact on later work** — hidden coupling: "changing A requires updating B or the
   index silently goes stale".

Never include any of the following, even though they are what a sub-task is mostly made of:

- **Restating the goal.** The goal lives in the frontmatter description as a short task identifier.
- **A step-by-step action log.** Keep only the turning points that changed the outcome; a transcript
  of what was tried in order is noise.
- **Code diffs or code snippets.** The code is the record.
- **File path or symbol inventories.** Anything derivable from the current project state (grep, git,
  the file tree) does not belong in memory — if it can be looked up, it must not be stored.
- **Anything the extraction prompt's four-item whitelist above does not cover.**`

// MemoryDriftCaveat is the recall-side drift caveat.
const MemoryDriftCaveat = `- Memory records can become stale over time. Use memory as context for what was true at a given point in time. Before answering the user or building assumptions based solely on information in memory records, verify that the memory is still correct and up-to-date by reading the current state of the files or resources. If a recalled memory conflicts with current information, trust what you observe now — and update or remove the stale memory rather than acting on it.`

// WhenToAccessSection is the `## When to access memories` section.
var WhenToAccessSection = `## When to access memories
- When memories seem relevant, or the user references prior-conversation work.
- You MUST access memory when the user explicitly asks you to check, recall, or remember.
- If the user says to *ignore* or *not use* memory: proceed as if MEMORY.md were empty. Do not apply remembered facts, cite, compare against, or mention memory content.
` + MemoryDriftCaveat

// TrustingRecallSection is the `## Before recommending from memory` section.
const TrustingRecallSection = `## Before recommending from memory

A memory that names a specific function, file, or flag is a claim that it existed *when the memory was written*. It may have been renamed, removed, or never merged. Before recommending it:

- If the memory names a file path: check the file exists.
- If the memory names a function or flag: grep for it.
- If the user is about to act on your recommendation (not just asking about history), verify first.

"The memory says X exists" is not the same as "X exists now."

A memory that summarizes repo state (activity logs, architecture snapshots) is frozen in time. If the user asks about *recent* or *current* state, prefer ` + "`git log`" + ` or reading the code over recalling the snapshot.`

// MemoryFrontmatterExample shows the frontmatter format including the
// `type` field. Used in How-to-save instructions.
const MemoryFrontmatterExample = "```markdown\n" +
	`---
name: {{memory name}}
description: {{one-line description — used to decide relevance in future conversations, so be specific}}
type: {{user, feedback, project, reference}}
---

{{memory content — for feedback/project types, structure as: rule/fact, then **Why:** and **How to apply:** lines}}
` + "```"

