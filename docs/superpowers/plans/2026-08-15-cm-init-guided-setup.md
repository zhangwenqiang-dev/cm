# `cm init` Guided Setup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the placeholder-heavy `cm init` output with a rerunnable guided initializer that safely configures the shared server, optional member token, local PEM, SSH user, and AI Skill.

**Architecture:** Keep command orchestration in `app_init.go`, move YAML-preserving document updates into a focused `init_config.go`, and isolate PEM discovery and token validation behind small testable helpers. Existing config files are parsed into a YAML node tree so targeted updates preserve Profiles, comments, and unknown fields; no-change reruns reuse the original bytes. New files are written atomically with private permissions.

**Tech Stack:** Go 1.25, `net/http`, `os`, `golang.org/x/term` for hidden terminal input, `go.yaml.in/yaml/v3` for comment-preserving structured YAML updates, existing ConnectMac remote Profile API and Skill manager.

---

## File Structure

- Create `internal/connectmac/init_config.go`: minimal config document model, YAML node lookup/update, preservation tracking, and private atomic writes.
- Create `internal/connectmac/app_init_test.go`: first-run, rerun, PEM, token, preservation, and error-path tests.
- Modify `internal/connectmac/app_init.go`: unified guided workflow for `cm init` and `cm init wizard`.
- Modify `internal/connectmac/app.go`: injectable secret reader used by tests and interactive terminals.
- Modify `internal/connectmac/app_diagnostics.go`: reuse a token-specific remote Profile request for validation and normal remote loading.
- Modify `internal/connectmac/app_usage.go`: clarify that initialization is guided and rerunnable.
- Modify `README.md`: replace manual-edit and placeholder Profile instructions in Quick Start.
- Modify `go.mod` and `go.sum`: direct dependencies for terminal secret input and structured YAML updates.

## Dependency Decision

- `golang.org/x/term`: maintained by the Go project, BSD-3-Clause, already present transitively in `go.sum`; used only to disable terminal echo while reading a token. The alternatives are visible token input or platform-specific termios code, both worse for security or maintenance. Binary and runtime impact are negligible for this CLI path.
- `go.yaml.in/yaml/v3`: maintained YAML v3 package, MIT/Apache-2.0, used to preserve unknown mappings and comments while updating only `server.user_api`, `server.token`, `defaults.user`, and `defaults.identity_file`. The alternative is line-based YAML surgery, which is fragile around quoting, comments, and nested mappings. It runs only during initialization, so runtime impact elsewhere is nil.

### Task 1: Add a YAML-Preserving Init Config Document

**Files:**
- Create: `internal/connectmac/init_config.go`
- Create: `internal/connectmac/app_init_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Add failing preservation and minimal-render tests**

Create tests that define the required document contract:

```go
func TestInitConfigDocumentMinimalFirstRun(t *testing.T) {
    doc, err := newInitConfigDocument(nil)
    if err != nil {
        t.Fatal(err)
    }
    doc.SetServerUserAPI(DefaultConnectMacServer)
    doc.SetDefaultUser(DefaultAWSUser)

    got, changed, err := doc.Bytes()
    if err != nil {
        t.Fatal(err)
    }
    if !changed {
        t.Fatal("new document must be changed")
    }
    want := "server:\n  user_api: https://cm.hsgitlab.xyz\ndefaults:\n  user: ec2-user\n"
    if string(got) != want {
        t.Fatalf("config = %q, want %q", got, want)
    }
}

func TestInitConfigDocumentPreservesProfilesCommentsAndUnknownFields(t *testing.T) {
    original := []byte(`# keep this comment
server:
  user_api: https://custom.example.com
custom_top_level:
  enabled: true
profiles:
  operations-use2:
    identity_file: ~/.ssh/eonebill-xcode.pem
`)
    doc, err := newInitConfigDocument(original)
    if err != nil {
        t.Fatal(err)
    }
    doc.SetServerToken("cm_api_new")
    got, changed, err := doc.Bytes()
    if err != nil {
        t.Fatal(err)
    }
    if !changed {
        t.Fatal("token insertion must mark document changed")
    }
    for _, want := range []string{"# keep this comment", "custom_top_level:", "operations-use2:", "identity_file: ~/.ssh/eonebill-xcode.pem", "token: cm_api_new"} {
        if !strings.Contains(string(got), want) {
            t.Fatalf("updated config missing %q:\n%s", want, got)
        }
    }
}

func TestInitConfigDocumentNoChangeReturnsOriginalBytes(t *testing.T) {
    original := []byte("server: {user_api: https://cm.hsgitlab.xyz}\n")
    doc, err := newInitConfigDocument(original)
    if err != nil {
        t.Fatal(err)
    }
    got, changed, err := doc.Bytes()
    if err != nil {
        t.Fatal(err)
    }
    if changed || !bytes.Equal(got, original) {
        t.Fatalf("no-change bytes changed: %q", got)
    }
}
```

- [ ] **Step 2: Run the focused tests and verify they fail**

Run:

```bash
go test ./internal/connectmac -run 'TestInitConfigDocument' -count=1
```

Expected: compilation fails because `newInitConfigDocument` and `DefaultConnectMacServer` do not exist.

- [ ] **Step 3: Add the two focused dependencies**

Run:

```bash
go get golang.org/x/term@v0.44.0 go.yaml.in/yaml/v3@v3.0.5
go mod tidy
```

Expected: `go.mod` lists both as direct dependencies and `go.sum` contains their checksums.

- [ ] **Step 4: Implement the document boundary**

Create these focused APIs in `init_config.go`:

```go
const DefaultConnectMacServer = "https://cm.hsgitlab.xyz"

type initConfigDocument struct {
    original []byte
    root     yaml.Node
    changed  bool
}

func newInitConfigDocument(data []byte) (*initConfigDocument, error)
func (d *initConfigDocument) ServerUserAPI() string
func (d *initConfigDocument) ServerToken() string
func (d *initConfigDocument) DefaultUser() string
func (d *initConfigDocument) DefaultIdentityFile() string
func (d *initConfigDocument) SetServerUserAPI(value string)
func (d *initConfigDocument) SetServerToken(value string)
func (d *initConfigDocument) SetDefaultUser(value string)
func (d *initConfigDocument) SetDefaultIdentityFile(value string)
func (d *initConfigDocument) Bytes() ([]byte, bool, error)
func writePrivateFileAtomic(path string, data []byte) error
```

Implementation requirements:

```go
func (d *initConfigDocument) setScalar(section, key, value string) {
    if value == "" || d.scalar(section, key) == value {
        return
    }
    mapping := ensureMapping(&d.root, section)
    setMappingScalar(mapping, key, value)
    d.changed = true
}

func (d *initConfigDocument) Bytes() ([]byte, bool, error) {
    if !d.changed && d.original != nil {
        return append([]byte(nil), d.original...), false, nil
    }
    var out bytes.Buffer
    encoder := yaml.NewEncoder(&out)
    encoder.SetIndent(2)
    if err := encoder.Encode(d.root.Content[0]); err != nil {
        return nil, false, err
    }
    if err := encoder.Close(); err != nil {
        return nil, false, err
    }
    return out.Bytes(), true, nil
}
```

`writePrivateFileAtomic` must create the parent directory with `0700`, write and sync a temporary file with `0600`, close it, and rename it over the destination. It must remove the temporary file on every failure.

- [ ] **Step 5: Run document tests**

Run:

```bash
go test ./internal/connectmac -run 'TestInitConfigDocument' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the document layer**

```bash
git add go.mod go.sum internal/connectmac/init_config.go internal/connectmac/app_init_test.go
git commit -m "feat: add safe init config updates"
```

### Task 2: Add PEM Discovery and Secure Token Validation

**Files:**
- Modify: `internal/connectmac/app.go`
- Modify: `internal/connectmac/app_diagnostics.go`
- Modify: `internal/connectmac/app_init_test.go`
- Create: `internal/connectmac/init_inputs.go`

- [ ] **Step 1: Add failing helper tests**

Add tests for deterministic PEM discovery and token authentication:

```go
func TestDiscoverInitPEMFilesOnlyReturnsRegularPEMs(t *testing.T) {
    sshDir := t.TempDir()
    writeFile(t, filepath.Join(sshDir, "maiqi-xcode.pem"), "key")
    writeFile(t, filepath.Join(sshDir, "ignore.txt"), "no")
    if err := os.Mkdir(filepath.Join(sshDir, "directory.pem"), 0o700); err != nil {
        t.Fatal(err)
    }
    got, err := discoverInitPEMFiles(sshDir)
    if err != nil {
        t.Fatal(err)
    }
    want := []string{"~/.ssh/maiqi-xcode.pem"}
    if !reflect.DeepEqual(got, want) {
        t.Fatalf("PEMs = %#v, want %#v", got, want)
    }
}

func TestValidateInitTokenUsesManagedProfilesEndpoint(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/api/managed-profiles" || r.URL.Query().Get("include_yaml") != "1" {
            t.Fatalf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
        }
        if r.Header.Get("Authorization") != "Bearer cm_api_valid" {
            writeWebError(w, http.StatusUnauthorized, "login required")
            return
        }
        writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]any{"profiles": []webManagedProfile{}}})
    }))
    defer server.Close()

    app := testApp(io.Discard, io.Discard, t.TempDir())
    if err := app.validateInitToken(context.Background(), server.URL, "cm_api_valid"); err != nil {
        t.Fatal(err)
    }
}
```

Also add a test where the endpoint returns `401` and assert validation fails without exposing the token in the error.

- [ ] **Step 2: Run the helper tests and verify they fail**

Run:

```bash
go test ./internal/connectmac -run 'TestDiscoverInitPEMFiles|TestValidateInitToken' -count=1
```

Expected: compilation fails because the helper functions do not exist.

- [ ] **Step 3: Implement input helpers and injectable secret reading**

Add this field and default in `App`/`NewApp`:

```go
ReadSecret func(prompt string, in io.Reader, out io.Writer) (string, error)
```

The default implementation in `init_inputs.go` should:

```go
func readInitSecret(prompt string, in io.Reader, out io.Writer) (string, error) {
    fmt.Fprint(out, prompt)
    file, ok := in.(*os.File)
    if ok && term.IsTerminal(int(file.Fd())) {
        value, err := term.ReadPassword(int(file.Fd()))
        fmt.Fprintln(out)
        return strings.TrimSpace(string(value)), err
    }
    fmt.Fprintln(out, "warning: terminal echo cannot be disabled; input will be visible")
    line, err := readInputLine(in)
    return strings.TrimSpace(line), err
}
```

Implement `discoverInitPEMFiles(sshDir string)` using `os.ReadDir`, `entry.Info()`, `Mode().IsRegular()`, a case-insensitive `.pem` suffix check, and sorted output. A missing `~/.ssh` directory returns an empty list rather than an error.

- [ ] **Step 4: Extract reusable token-authenticated remote loading**

Refactor `app_diagnostics.go` so both normal loading and initialization call:

```go
func (a App) fetchRemoteProfilesWithToken(ctx context.Context, userAPI, token string) ([]webManagedProfile, error)

func (a App) validateInitToken(ctx context.Context, userAPI, token string) error {
    _, err := a.fetchRemoteProfilesWithToken(ctx, userAPI, token)
    return err
}
```

Keep the existing endpoint, Bearer header, response parser, and public error wrapping. Never include the token in an error message.

- [ ] **Step 5: Run focused and existing remote-list tests**

Run:

```bash
go test ./internal/connectmac -run 'TestDiscoverInitPEMFiles|TestValidateInitToken|TestAppListFetchesRemoteProfiles' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit input and validation helpers**

```bash
git add internal/connectmac/app.go internal/connectmac/app_diagnostics.go internal/connectmac/init_inputs.go internal/connectmac/app_init_test.go
git commit -m "feat: add secure init inputs"
```

### Task 3: Replace `cm init` With the Rerunnable Guided Flow

**Files:**
- Modify: `internal/connectmac/app_init.go`
- Modify: `internal/connectmac/app_init_test.go`
- Modify: `internal/connectmac/app_test.go`

- [ ] **Step 1: Replace the outdated init/list test with failing guided-flow tests**

Remove the expectation that initialization creates `xcode-vnc`. Add tests using `bytes.Buffer` input:

```go
func TestAppInitCreatesMinimalConfigWhenOptionalInputsSkipped(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "config.yaml")
    var out, errOut bytes.Buffer
    app := testApp(&out, &errOut, dir)
    app.In = strings.NewReader("0\n\nn\n")

    if code := app.Run(context.Background(), []string{"init", "--config", path}); code != 0 {
        t.Fatalf("init code = %d, err = %s", code, errOut.String())
    }
    data, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }
    text := string(data)
    for _, want := range []string{"user_api: https://cm.hsgitlab.xyz", "user: ec2-user"} {
        if !strings.Contains(text, want) {
            t.Fatalf("config missing %q:\n%s", want, text)
        }
    }
    for _, unwanted := range []string{"xcode-vnc", "example.pem", "amis_by_region", "elastic_ip"} {
        if strings.Contains(text, unwanted) {
            t.Fatalf("config contains obsolete value %q:\n%s", unwanted, text)
        }
    }
}

func TestAppInitRerunPreservesExistingServerProfilesAndComments(t *testing.T) {
    original := `# member config
server:
  user_api: https://custom.example.com
  token: cm_api_existing
defaults:
  user: custom-user
profiles:
  operations-use2:
    identity_file: ~/.ssh/eonebill-xcode.pem
`
    path := filepath.Join(t.TempDir(), "config.yaml")
    writeFile(t, path, original)
    app := testApp(io.Discard, io.Discard, t.TempDir())
    app.In = strings.NewReader("n\n")

    if code := app.Run(context.Background(), []string{"init", "--config", path}); code != 0 {
        t.Fatalf("init code = %d", code)
    }
    got, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }
    if string(got) != original {
        t.Fatalf("configured rerun changed file:\n%s", got)
    }
}
```

Add separate tests for:

- choosing a discovered PEM writes `defaults.identity_file`;
- missing token accepts a validated token;
- rejected token is not written and can be skipped;
- missing `server.user_api` is automatically added while existing Profiles survive;
- malformed YAML returns code `1` and leaves bytes unchanged;
- `cm init wizard` invokes the same behavior;
- an existing server token is neither prompted nor printed.

- [ ] **Step 2: Run the guided tests and verify they fail**

Run:

```bash
go test ./internal/connectmac -run 'TestAppInit' -count=1
```

Expected: tests fail because the old command exits on an existing file and writes the old template.

- [ ] **Step 3: Implement one unified initializer**

Refactor `runInit` and `runInitWizard` to call:

```go
func (a App) runGuidedInit(ctx context.Context, configPath string) int
```

Pass the existing command context from `App.Run` by changing the dispatch to:

```go
case "init":
    return a.runInit(ctx, configPath, args[1:])
```

The orchestration order must be:

```go
// 1. Expand and read existing bytes; os.IsNotExist means first run.
// 2. Parse with newInitConfigDocument; stop before prompts on invalid YAML.
// 3. Add DefaultConnectMacServer only when user_api is missing.
// 4. Add DefaultAWSUser only when defaults.user is missing.
// 5. If identity_file is missing, discover PEMs and prompt for a numbered choice; allow 0/empty.
// 6. If token is missing, read it securely; empty skips; validate before SetServerToken.
// 7. Write only when the document reports changed=true.
// 8. Offer optional a.runSkillSetup(nil).
// 9. Print checks and next actions without printing the token.
```

Use small helpers in `app_init.go`:

```go
func (a App) chooseInitPEM(paths []string) string
func (a App) promptInitToken(ctx context.Context, userAPI string) string
func (a App) printInitSummary(path string, doc *initConfigDocument, pemReadable bool)
```

Token rejection should print a concise error and prompt `Retry token entry? [y/N]:`; choosing no leaves it unset and initialization still succeeds.

- [ ] **Step 4: Remove the obsolete template**

Delete the placeholder-heavy `DefaultConfigTemplate()` implementation. If tests or callers still need a template function, retain only:

```go
func DefaultConfigTemplate() string {
    return "server:\n  user_api: https://cm.hsgitlab.xyz\ndefaults:\n  user: ec2-user\n"
}
```

No dummy Profile, AMI, subnet, security group, EIP, VNC user, host, or PEM may remain in first-run output.

- [ ] **Step 5: Run initializer tests**

Run:

```bash
go test ./internal/connectmac -run 'TestAppInit|TestInitConfigDocument|TestDiscoverInitPEMFiles|TestValidateInitToken' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the guided workflow**

```bash
git add internal/connectmac/app.go internal/connectmac/app_init.go internal/connectmac/app_init_test.go internal/connectmac/app_test.go
git commit -m "feat: make cm init guided and rerunnable"
```

### Task 4: Update Help and Quick Start Documentation

**Files:**
- Modify: `internal/connectmac/app_usage.go`
- Modify: `internal/connectmac/app_completion_skill_test.go`
- Modify: `README.md`

- [ ] **Step 1: Add a failing usage/documentation assertion**

Extend the usage test to require guided-init language and keep `init wizard` compatibility:

```go
func TestUsageDescribesGuidedInit(t *testing.T) {
    var out, errOut bytes.Buffer
    app := testApp(&out, &errOut, t.TempDir())
    if code := app.Run(context.Background(), []string{"--help"}); code != 0 {
        t.Fatalf("help code = %d", code)
    }
    text := out.String()
    for _, want := range []string{"cm init", "guided", "cm init wizard"} {
        if !strings.Contains(text, want) {
            t.Fatalf("help missing %q: %s", want, text)
        }
    }
}
```

- [ ] **Step 2: Run the usage test and verify it fails**

Run:

```bash
go test ./internal/connectmac -run 'TestUsageDescribesGuidedInit' -count=1
```

Expected: FAIL because help does not describe guided setup.

- [ ] **Step 3: Update help and README**

Add one concise help line after the usage block:

```text
cm init is a guided, rerunnable setup for the server, local PEM, member token, and AI Skill.
```

Replace the README Quick Start with:

```markdown
## Quick Start

Run the guided initializer:

```bash
cm init
```

The initializer configures `https://cm.hsgitlab.xyz`, discovers PEM files in `~/.ssh`, optionally stores a member token, and can install the ConnectMac AI Skill. Token and PEM steps may be skipped; rerun `cm init` later to complete missing settings.

Shared Profiles come from the ConnectMac server. Local configuration stores member-specific values such as the default PEM and optional per-Profile PEM overrides.
```

Keep the advanced configuration reference, but remove language that tells every member to manually create the sample `xcode-vnc` Profile.

- [ ] **Step 4: Run documentation-facing tests**

Run:

```bash
go test ./internal/connectmac -run 'TestUsage|TestCompletionCommandsUseSkill|TestAppInit' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit docs and help**

```bash
git add README.md internal/connectmac/app_usage.go internal/connectmac/app_completion_skill_test.go
git commit -m "docs: update guided init instructions"
```

### Task 5: Full Regression and Manual Workflow Verification

**Files:**
- Modify only if verification exposes a defect in files already listed above.

- [ ] **Step 1: Format and run static checks**

Run:

```bash
gofmt -w internal/connectmac/app.go internal/connectmac/app_init.go internal/connectmac/app_init_test.go internal/connectmac/app_diagnostics.go internal/connectmac/init_config.go internal/connectmac/init_inputs.go
go vet ./...
```

Expected: no output from `gofmt`; `go vet` exits `0`.

- [ ] **Step 2: Run the complete test suite**

Run:

```bash
go test ./... -count=1
```

Expected: all packages PASS.

- [ ] **Step 3: Build a temporary binary**

Run:

```bash
go build -o /private/tmp/cm-init-verify ./cmd/cm
```

Expected: exit `0` and `/private/tmp/cm-init-verify` exists.

- [ ] **Step 4: Verify a skipped first run in an isolated HOME**

Run interactively with a temporary home:

```bash
HOME=/private/tmp/cm-init-home /private/tmp/cm-init-verify init
```

Choose `0` for PEM, press Enter to skip Token, and answer `n` for Skill. Expected:

- `~/.connectmac/config.yaml` is created under the temporary HOME.
- It contains only the standard server and `ec2-user`.
- The summary marks Token and PEM as skipped.
- No placeholder Profile or AWS resource appears.

- [ ] **Step 5: Verify a rerun is non-destructive**

Record the checksum, rerun with all optional actions skipped, and compare:

```bash
shasum -a 256 /private/tmp/cm-init-home/.connectmac/config.yaml
HOME=/private/tmp/cm-init-home /private/tmp/cm-init-verify init
shasum -a 256 /private/tmp/cm-init-home/.connectmac/config.yaml
```

Expected: checksums match when no required field is missing and no option is selected.

- [ ] **Step 6: Check the final diff and repository hygiene**

Run:

```bash
git diff --check
git status --short
```

Expected: no whitespace errors. Unrelated existing untracked files `.mcp.json`, `.superpowers/`, `CLAUDE.md`, and `web-ui/` remain untouched and uncommitted.

- [ ] **Step 7: Commit any verification-only fixes**

Only if Step 1-6 required a correction:

```bash
git add go.mod go.sum README.md internal/connectmac/app.go internal/connectmac/app_init.go internal/connectmac/app_init_test.go internal/connectmac/app_diagnostics.go internal/connectmac/app_usage.go internal/connectmac/app_completion_skill_test.go internal/connectmac/init_config.go internal/connectmac/init_inputs.go
git commit -m "fix: complete guided init verification"
```

Expected: a clean tracked worktree after the commit; unrelated untracked files remain present.
