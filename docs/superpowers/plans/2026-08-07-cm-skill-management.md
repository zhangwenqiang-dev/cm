# CM Skill Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `cm init-rules` with a safe `cm skill` command group that manages the built-in ConnectMac skill and Agent rule blocks.

**Architecture:** Keep embedded templates and marker helpers in `rules.go`, put lifecycle and hash decisions in `skill_manager.go`, and expose them through `app_skill.go`. Existing `cm init` reuses the setup service. Managed state lives in `~/.connectmac/skill-state.json`; modified installations require a verified backup before force update.

**Tech Stack:** Go standard library, existing ConnectMac dispatcher/completion generators, Go `testing`.

---

## File Structure

- Create `internal/connectmac/skill_manager.go`: status, state, install, update, backup, validation, uninstall.
- Create `internal/connectmac/skill_manager_test.go`: lifecycle and safety tests.
- Create `internal/connectmac/app_skill.go`: command parsing, dispatch, output, exit codes.
- Create `internal/connectmac/app_skill_test.go`: public command tests.
- Modify `internal/connectmac/rules.go`: retain templates/setup and add conservative block removal.
- Modify `internal/connectmac/app_init.go` and `app.go`: remove `init-rules`, register/reuse `skill`.
- Modify `internal/connectmac/app_usage.go` and `app_completion.go`: help and shell completion.
- Modify `internal/connectmac/guidance.go`, `README.md`, and related tests: documentation migration.

### Task 1: Skill State and Status Model

**Files:**
- Create: `internal/connectmac/skill_manager.go`
- Create: `internal/connectmac/skill_manager_test.go`

- [ ] **Step 1: Write failing status tests**

Create table-driven tests for `missing`, `current`, `outdated`, `modified`, and `invalid`. Also verify matching legacy files without state are `current`, while non-matching legacy files are `modified`.

Use these public types in assertions:

```go
type SkillStatusName string

const (
    SkillStatusMissing  SkillStatusName = "missing"
    SkillStatusCurrent  SkillStatusName = "current"
    SkillStatusOutdated SkillStatusName = "outdated"
    SkillStatusModified SkillStatusName = "modified"
    SkillStatusInvalid  SkillStatusName = "invalid"
)
```

- [ ] **Step 2: Verify tests fail**

Run: `go test ./internal/connectmac -run 'TestSkillManagerStatus' -count=1`

Expected: compile failure because the manager and status constants do not exist.

- [ ] **Step 3: Implement read-only state/status**

Implement:

```go
type SkillState struct {
    SchemaVersion    int       `json:"schema_version"`
    SkillName        string    `json:"skill_name"`
    SkillPath        string    `json:"skill_path"`
    CMVersion        string    `json:"cm_version"`
    SkillSHA256      string    `json:"skill_sha256"`
    OpenAIYAMLSHA256 string    `json:"openai_yaml_sha256"`
    InstalledAt      time.Time `json:"installed_at"`
    UpdatedAt        time.Time `json:"updated_at"`
}

type SkillManager struct {
    SkillPath string
    StatePath string
    Version   string
    Now       func() time.Time
}
```

Hash exact embedded bytes with SHA-256. Distinguish malformed/unreadable state from missing files.

- [ ] **Step 4: Run status tests**

Run: `go test ./internal/connectmac -run 'TestSkillManagerStatus' -count=1`

Expected: PASS.

### Task 2: Safe Lifecycle Operations

**Files:**
- Modify: `internal/connectmac/skill_manager.go`
- Modify: `internal/connectmac/skill_manager_test.go`
- Modify: `internal/connectmac/rules.go`

- [ ] **Step 1: Write failing lifecycle tests**

Add tests for fresh install, matching legacy adoption, modified-file refusal, ordinary update, force-update backup, backup failure preserving originals, idempotent uninstall, and rule-block removal preserving unrelated content.

- [ ] **Step 2: Verify lifecycle tests fail**

Run: `go test ./internal/connectmac -run 'TestSkillManager(Install|Update|Force|Uninstall)|TestRemoveMarked' -count=1`

Expected: FAIL because lifecycle methods do not exist.

- [ ] **Step 3: Implement lifecycle methods**

Implement:

```go
type SkillActionResult struct {
    Status     SkillStatus
    BackupPath string
    Changed    bool
}

func (m SkillManager) Install(dryRun bool) (SkillActionResult, error)
func (m SkillManager) Update(force, dryRun bool) (SkillActionResult, error)
func (m SkillManager) Uninstall(dryRun bool) (SkillActionResult, error)
func (m SkillManager) Validate() error
```

Write through same-directory temporary files followed by rename. Write state after both skill files. Back up modified content to `~/.connectmac/backups/skills/connectmac-YYYYMMDD-HHMMSS` and verify copied hashes before replacement.

- [ ] **Step 4: Implement conservative rule removal**

Add:

```go
func removeMarkedRulesBlock(content string) (updated string, found bool)
func UninstallRulesBlock(agent, projectDir string, dryRun bool) (path string, changed bool, err error)
```

Only remove current/legacy ConnectMac markers. Never delete unrelated content. Remove an empty Cursor managed file; retain AGENTS.md and CLAUDE.md.

- [ ] **Step 5: Run lifecycle and existing rule tests**

Run: `go test ./internal/connectmac -run 'SkillManager|MarkedRules|InstallRules|BuildRules' -count=1`

Expected: PASS.

### Task 3: Public `cm skill` Commands

**Files:**
- Create: `internal/connectmac/app_skill.go`
- Create: `internal/connectmac/app_skill_test.go`
- Modify: `internal/connectmac/app.go`
- Modify: `internal/connectmac/app_init.go`
- Modify: `internal/connectmac/rules.go`
- Modify: `internal/connectmac/rules_test.go`

- [ ] **Step 1: Write failing command tests**

Cover all subcommands, setup for four Agents, dry-run, path-only output, print without writes, force-update backup, uninstall `--rules` validation, unknown options, and rejection of `cm init-rules`.

- [ ] **Step 2: Verify command tests fail**

Run: `go test ./internal/connectmac -run 'TestAppSkill|TestAppInitRulesIsUnknown' -count=1`

Expected: FAIL because `skill` is not registered.

- [ ] **Step 3: Implement strict parsing and dispatch**

Create:

```go
func (a App) runSkill(args []string) int
func (a App) runSkillSetup(args []string) int
func parseSkillPathOptions(args []string) (SkillPathOptions, error)
func parseSkillSetupOptions(args []string) (SkillSetupOptions, error)
func parseSkillUpdateOptions(args []string) (SkillUpdateOptions, error)
func parseSkillUninstallOptions(args []string) (SkillUninstallOptions, error)
```

Resolve default paths from HOME, pass `a.Version` to the manager, return exit `2` for usage errors and `1` for operation errors.

- [ ] **Step 4: Replace old command and init integration**

Register `skill` in `App.Run`, remove `init-rules`, delete `runInitRules`, and make both init prompts call `runSkillSetup(nil)`.

- [ ] **Step 5: Run command/init tests**

Run: `go test ./internal/connectmac -run 'TestAppSkill|TestAppInit|TestInstallRules|TestBuildRules' -count=1`

Expected: PASS.

### Task 4: Usage and Completion

**Files:**
- Modify: `internal/connectmac/app_usage.go`
- Modify: `internal/connectmac/app_completion.go`
- Modify or create: `internal/connectmac/app_completion_test.go`

- [ ] **Step 1: Write failing usage/completion tests**

Assert top-level commands contain `skill`, omit `init-rules`, and expose exactly:

```go
[]string{"setup", "install", "status", "update", "validate", "path", "print", "uninstall"}
```

Assert Zsh, Bash, and Fish route `cm skill <TAB>` through the new completion target.

- [ ] **Step 2: Verify completion tests fail**

Run: `go test ./internal/connectmac -run 'Usage.*Skill|Completion.*Skill' -count=1`

Expected: FAIL because old completion is still active.

- [ ] **Step 3: Implement help/completion migration**

Add `skill-commands` dispatch and `completionSkillCommands()`; update all three shell generators and top-level usage.

- [ ] **Step 4: Run usage/completion tests**

Run: `go test ./internal/connectmac -run 'Usage|Completion' -count=1`

Expected: PASS.

### Task 5: Guides and README

**Files:**
- Modify: `internal/connectmac/guidance.go`
- Modify or create: `internal/connectmac/guidance_test.go`
- Modify: `README.md`

- [ ] **Step 1: Write failing guide test**

Assert the first-use guide contains `cm skill setup --agent codex --project .` and contains no active `cm init-rules` instruction.

- [ ] **Step 2: Verify test fails**

Run: `go test ./internal/connectmac -run TestFirstUseGuideUsesSkillSetup -count=1`

Expected: FAIL.

- [ ] **Step 3: Migrate documentation**

Document setup, status, update, force backup, `--skills-dir`, conservative uninstall, and package-upgrade behavior. Do not rewrite historical design/release documents.

- [ ] **Step 4: Check active references**

Run: `rg -n "cm init-rules|init-rules" README.md internal/connectmac`

Expected: only the intentional negative removal test may match.

- [ ] **Step 5: Run guide tests**

Run: `go test ./internal/connectmac -run 'Guide|FirstUse' -count=1`

Expected: PASS.

### Task 6: Full Verification

**Files:**
- Modify only files required by failures discovered here.

- [ ] **Step 1: Format changed Go files**

Run: `gofmt -w internal/connectmac/skill_manager.go internal/connectmac/skill_manager_test.go internal/connectmac/app_skill.go internal/connectmac/app_skill_test.go internal/connectmac/rules.go internal/connectmac/rules_test.go internal/connectmac/app.go internal/connectmac/app_init.go internal/connectmac/app_usage.go internal/connectmac/app_completion.go internal/connectmac/app_completion_test.go internal/connectmac/guidance.go internal/connectmac/guidance_test.go`

Expected: command succeeds.

- [ ] **Step 2: Run focused package tests**

Run: `go test ./internal/connectmac -count=1`

Expected: PASS.

- [ ] **Step 3: Run full repository tests**

Run: `go test ./... -count=1`

Expected: PASS.

- [ ] **Step 4: Check scope and stale references**

Run:

```bash
git diff --check
rg -n "cm init-rules|init-rules" README.md internal/connectmac
git status --short
```

Expected: no whitespace errors; only an intentional removal test may mention the old command; unrelated pre-existing untracked files remain untouched.

- [ ] **Step 5: Commit implementation**

Stage only files from this plan and commit with a concise feature message. Do not stage `.mcp.json`, `.superpowers/`, `CLAUDE.md`, or `web-ui/`.

