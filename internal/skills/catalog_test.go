// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSkillDir(t *testing.T, root, name, frontmatter, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

func TestLoadCatalogPhase1EmptyBody(t *testing.T) {
	work := t.TempDir()
	writeSkillDir(t, filepath.Join(work, ".tencoder", "skills"), "demo",
		"name: demo\ndescription: a phase-1 demo\nmode: inline",
		"# Body\n\nFull SOP here.")

	cat := LoadCatalog(work)
	skill := cat.Get("demo")
	if skill == nil {
		t.Fatal("expected demo skill in phase-1 catalog")
	}
	if skill.PromptBody != "" {
		t.Errorf("phase-1 must NOT load body; got %d chars", len(skill.PromptBody))
	}
	if skill.BodyLoaded {
		t.Errorf("BodyLoaded must be false in phase-1")
	}
	if skill.Meta.Description != "a phase-1 demo" {
		t.Errorf("frontmatter not parsed: %q", skill.Meta.Description)
	}
}

func TestCatalogGetFullHotReload(t *testing.T) {
	work := t.TempDir()
	dir := writeSkillDir(t, filepath.Join(work, ".tencoder", "skills"), "hot",
		"name: hot\ndescription: hot reload demo",
		"version 1")

	cat := LoadCatalog(work)
	skill, err := cat.GetFull("hot")
	if err != nil {
		t.Fatalf("GetFull v1: %v", err)
	}
	if !strings.Contains(skill.PromptBody, "version 1") {
		t.Errorf("v1 body mismatch: %q", skill.PromptBody)
	}

	// Overwrite source file — GetFull must re-read.
	updated := "---\nname: hot\ndescription: hot reload demo\n---\n\nversion 2"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(updated), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	skill2, err := cat.GetFull("hot")
	if err != nil {
		t.Fatalf("GetFull v2: %v", err)
	}
	if !strings.Contains(skill2.PromptBody, "version 2") {
		t.Errorf("hot reload didn't pick up v2: %q", skill2.PromptBody)
	}
}

func TestCatalogNeedsReload(t *testing.T) {
	work := t.TempDir()
	skillsDir := filepath.Join(work, ".tencoder", "skills")
	writeSkillDir(t, skillsDir, "alpha",
		"name: alpha\ndescription: first skill", "body alpha")

	cat := LoadCatalog(work)
	if cat.NeedsReload() {
		t.Error("NeedsReload should be false right after LoadCatalog")
	}

	// Ensure filesystem tick so modtime differs (ext4 can have 1s granularity).
	time.Sleep(10 * time.Millisecond)

	// Add a new skill directory — parent modtime changes.
	writeSkillDir(t, skillsDir, "beta",
		"name: beta\ndescription: second skill", "body beta")

	if !cat.NeedsReload() {
		t.Error("NeedsReload should be true after adding a new skill dir")
	}

	// After reload, NeedsReload resets.
	cat.Reload(work)
	if cat.NeedsReload() {
		t.Error("NeedsReload should be false after Reload")
	}
	if cat.Get("beta") == nil {
		t.Error("beta should be in catalog after Reload")
	}
}

func TestLoadCatalogNoBuiltins(t *testing.T) {
	cat := LoadCatalog(t.TempDir())
	if len(cat.List()) != 0 {
		t.Errorf("expected empty catalog with no disk skills, got %d", len(cat.List()))
	}
}
