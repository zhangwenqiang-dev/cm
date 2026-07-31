# Login Challenge Script Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore login challenge loading and prevent parse-time errors in the inline web application script from reaching a release.

**Architecture:** Keep the existing single-file web application and authentication API unchanged. Add a small Node-based syntax checker that extracts inline scripts from `web/index.html` and compiles them without executing them, then invoke that checker from the Go test suite and the Makefile test target.

**Tech Stack:** HTML, browser JavaScript, Node.js syntax compilation, Go tests, Make.

---

### Task 1: Add a failing web script syntax check

**Files:**
- Create: `scripts/check-web-js.mjs`
- Create: `internal/connectmac/web_script_syntax_test.go`
- Modify: `Makefile`

- [ ] **Step 1: Create the syntax checker**

Create `scripts/check-web-js.mjs` with code that reads `web/index.html`,
extracts every inline `<script>` without a `src` attribute, and passes each
script to `new Function(source)`. On a syntax error, print the script number and
the parser error, then exit non-zero.

- [ ] **Step 2: Add a Go regression test**

Add `TestWebInlineScriptsParse` in
`internal/connectmac/web_script_syntax_test.go`. Resolve `node` with
`exec.LookPath`; skip only when Node.js is unavailable. Run
`../../scripts/check-web-js.mjs` from the package test directory and fail with
the combined output when the checker exits non-zero.

- [ ] **Step 3: Require the checker in the standard Makefile test path**

Update `Makefile`:

```make
test:
	node scripts/check-web-js.mjs
	GOCACHE=$(GOCACHE) go test ./...
```

- [ ] **Step 4: Run the checker and verify the regression fails**

Run:

```bash
node scripts/check-web-js.mjs
```

Expected: non-zero exit with
`Identifier 'body' has already been declared`.

### Task 2: Apply the minimal login initialization fix

**Files:**
- Modify: `web/index.html:1124-1147`

- [ ] **Step 1: Rename the parsed response variable**

Keep the request payload variable named `body`. Rename the response to
`responseBody` and update the error and return paths:

```javascript
const responseBody = await res.json();
if (!responseBody.ok) {
  const err = new Error((responseBody.error || "") +
    (responseBody.output ? "\n" + responseBody.output : ""));
  err.status = res.status;
  throw err;
}
return responseBody;
```

- [ ] **Step 2: Run the syntax checker**

Run:

```bash
node scripts/check-web-js.mjs
```

Expected: `web JavaScript syntax OK`.

- [ ] **Step 3: Run focused and complete tests**

Run:

```bash
go test ./internal/connectmac -run TestWebInlineScriptsParse -count=1
go test ./... -count=1
```

Expected: both commands pass.

### Task 3: Release and verify production

**Files:**
- Modify: `Formula/cm.rb`

- [ ] **Step 1: Increment the patch release**

Change the Formula tag from `v0.1.132` to `v0.1.133`.

- [ ] **Step 2: Commit and push**

Stage only the checker, test, Makefile, web fix, plan, and Formula. Commit with:

```bash
git commit -m "fix: restore login challenge initialization"
```

Push the working branch and fast-forward `main`.

- [ ] **Step 3: Publish packages**

Create and push `v0.1.133`, build `amd64` and `arm64` Debian packages, create
the GitHub Release, and verify both assets are present.

- [ ] **Step 4: Upgrade local and staging installations**

Upgrade Homebrew locally, restart the local agent, deploy the arm64 Debian
package to `staging2`, and verify both report `cm 0.1.133`.

- [ ] **Step 5: Verify the production login page**

Open `https://cm.hsgitlab.xyz/` in Chrome and verify:

- The challenge changes from `校验题加载中...` to an arithmetic question.
- Clicking `换一题` displays a new valid question.
- The console contains no JavaScript syntax error.
- `GET /api/auth/challenge` returns HTTP `200`.
