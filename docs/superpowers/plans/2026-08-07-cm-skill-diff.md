# `cm skill diff` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only `cm skill diff` command that renders unified differences between the installed ConnectMac skill and the skill embedded in the current binary.

**Architecture:** Keep diff generation in a focused `skill_diff.go` module. `SkillManager.Diff` reads the two managed installed files and compares them to the built-in templates, while `app_skill.go` handles option parsing, output, and exit codes. Existing installation, status, update, and state behavior remains unchanged.

**Tech Stack:** Go standard library, existing ConnectMac CLI dispatch and completion generators, Go tests, README Markdown.

---

### Task 1: Add the skill diff model and renderer

**Files:**
- Create: `internal/connectmac/skill_diff.go`
- Create: `internal/connectmac/skill_diff_test.go`

- [ ] **Step 1: Write failing renderer tests**

Add table-driven tests that assert identical text produces no output, one changed line produces headers and `-`/`+` lines, insertions and deletions retain context, and inputs without trailing newlines remain readable.

```go
func TestRenderUnifiedTextDiff(t *testing.T) {
	text, changed := renderUnifiedTextDiff("installed/SKILL.md", "built-in/SKILL.md", "old\n", "new\n")
	if !changed || !strings.Contains(text, "--- installed/SKILL.md") || !strings.Contains(text, "-old") || !strings.Contains(text, "+new") {
		t.Fatalf("diff = %q, changed = %v", text, changed)
	}
}
```

- [ ] **Step 2: Run the focused test and verify failure**

Run: `go test ./internal/connectmac -run 'TestRenderUnifiedTextDiff' -count=1`

Expected: compilation fails because `renderUnifiedTextDiff` does not exist.

- [ ] **Step 3: Implement a small line-level unified diff**

Define an internal edit type, compute the longest common subsequence for the small text inputs, convert it into equal/delete/insert edits, group changes with three context lines, and render standard unified headers and hunks.

```go
func renderUnifiedTextDiff(oldName, newName, oldText, newText string) (string, bool) {
	if oldText == newText {
		return "", false
	}
	edits := lineEdits(splitDiffLines(oldText), splitDiffLines(newText))
	return renderUnifiedHunks(oldName, newName, edits, 3), true
}
```

- [ ] **Step 4: Run focused renderer tests**

Run: `go test ./internal/connectmac -run 'TestRenderUnifiedTextDiff' -count=1`

Expected: PASS.

### Task 2: Add `SkillManager.Diff`

**Files:**
- Modify: `internal/connectmac/skill_diff.go`
- Modify: `internal/connectmac/skill_diff_test.go`

- [ ] **Step 1: Write failing manager tests**

Test missing installation, matching installation, modified `SKILL.md`, modified `agents/openai.yaml`, and preservation of installed files and state after the call.

```go
func TestSkillManagerDiffModifiedSkill(t *testing.T) {
	manager := newTestSkillManager(t)
	if _, err := manager.Install(false); err != nil { t.Fatal(err) }
	path := filepath.Join(manager.SkillPath, "SKILL.md")
	if err := os.WriteFile(path, []byte("manual\n"), 0o644); err != nil { t.Fatal(err) }
	result, err := manager.Diff()
	if err != nil || !result.Changed || !strings.Contains(result.Text, "installed/SKILL.md") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
```

- [ ] **Step 2: Run focused manager tests and verify failure**

Run: `go test ./internal/connectmac -run 'TestSkillManagerDiff' -count=1`

Expected: compilation fails because `SkillManager.Diff` does not exist.

- [ ] **Step 3: Implement the manager operation**

Add a result type and compare exactly the two managed files against `DefaultSkillTemplate()` and `DefaultSkillOpenAIYAML()`.

```go
type SkillDiffResult struct {
	Text    string
	Changed bool
}

func (m SkillManager) Diff() (SkillDiffResult, error) {
	// Require the installed skill directory, read both managed files,
	// render changed files in stable order, and never write state.
}
```

- [ ] **Step 4: Run focused manager tests**

Run: `go test ./internal/connectmac -run 'TestSkillManagerDiff' -count=1`

Expected: PASS.

### Task 3: Wire the CLI command

**Files:**
- Modify: `internal/connectmac/app_skill.go`
- Modify: `internal/connectmac/app_skill_test.go`

- [ ] **Step 1: Write failing CLI tests**

Cover matching output, modified unified output, missing skill exit code `1`, and `--skills-dir` parsing.

```go
if code := app.Run(context.Background(), []string{"skill", "diff", "--skills-dir", skillsDir}); code != 0 {
	t.Fatalf("diff code=%d err=%s", code, errOut.String())
}
```

- [ ] **Step 2: Run focused CLI tests and verify failure**

Run: `go test ./internal/connectmac -run 'TestAppSkillDiff' -count=1`

Expected: FAIL with unknown skill command `diff`.

- [ ] **Step 3: Add dispatch, output, and usage**

Parse `diff` with `parseSkillPathOptions(args, false)`, invoke `manager.Diff()`, print the diff when changed, otherwise print `connectmac skill matches the built-in version`, and add the command to `printSkillUsage`.

- [ ] **Step 4: Run focused CLI tests**

Run: `go test ./internal/connectmac -run 'TestAppSkillDiff' -count=1`

Expected: PASS.

### Task 4: Add completion and user documentation

**Files:**
- Modify: `internal/connectmac/app_completion.go`
- Modify: `internal/connectmac/app_completion_skill_test.go`
- Modify: `README.md`

- [ ] **Step 1: Update the completion expectation first**

Add `diff` between `status` and `update` in the expected skill command list, then run:

`go test ./internal/connectmac -run 'TestCompletionSkill' -count=1`

Expected: FAIL until the completion list changes.

- [ ] **Step 2: Add `diff` to generated completion**

Update `completionSkillCommands()` to return:

```go
[]string{"setup", "install", "status", "diff", "update", "validate", "path", "print", "uninstall"}
```

- [ ] **Step 3: Document review-before-force workflow**

Update the README command block to show:

```bash
cm skill status
cm skill diff
cm skill update
```

Explain that `diff` is read-only and returns success when differences are found.

- [ ] **Step 4: Run completion tests**

Run: `go test ./internal/connectmac -run 'TestCompletionSkill' -count=1`

Expected: PASS.

### Task 5: Full verification

**Files:**
- Verify all modified files.

- [ ] **Step 1: Format and inspect changes**

Run: `gofmt -w internal/connectmac/skill_diff.go internal/connectmac/skill_diff_test.go internal/connectmac/app_skill.go internal/connectmac/app_skill_test.go internal/connectmac/app_completion.go internal/connectmac/app_completion_skill_test.go`

Run: `git diff --check`

Expected: no output from `git diff --check`.

- [ ] **Step 2: Run static and complete test suites**

Run: `go vet ./...`

Run: `go test ./... -count=1`

Expected: PASS.

- [ ] **Step 3: Validate generated shell scripts**

Run: `go run ./cmd/cm completion zsh > /tmp/cm-skill-diff.zsh && zsh -n /tmp/cm-skill-diff.zsh`

Run: `go run ./cmd/cm completion bash > /tmp/cm-skill-diff.bash && bash -n /tmp/cm-skill-diff.bash`

Expected: both syntax checks pass and `go run ./cmd/cm completion skill-commands` contains `diff`.

- [ ] **Step 4: Perform a binary smoke test**

Install the skill into a temporary directory, confirm a matching result, modify `SKILL.md`, and confirm unified output without changing the file.

- [ ] **Step 5: Commit the implementation**

```bash
git add internal/connectmac/skill_diff.go internal/connectmac/skill_diff_test.go internal/connectmac/app_skill.go internal/connectmac/app_skill_test.go internal/connectmac/app_completion.go internal/connectmac/app_completion_skill_test.go README.md docs/superpowers/plans/2026-08-07-cm-skill-diff.md
git commit -m "feat: add cm skill diff"
```
