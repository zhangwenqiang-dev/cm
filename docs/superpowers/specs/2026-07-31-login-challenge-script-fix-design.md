# Login Challenge Script Fix Design

## Problem

The production login page remains on `校验题加载中...` even though
`GET /api/auth/challenge` is healthy. Chrome reports:

```text
SyntaxError: Identifier 'body' has already been declared
```

`localAgentAPI()` declares `let body` for the request payload and later declares
`const body` for the JSON response in the same function scope. The browser
rejects the entire inline script before execution, so login initialization and
`loadChallenge()` never run.

## Design

Apply the smallest behavioral fix:

- Keep `body` as the mutable request payload variable.
- Rename the parsed response variable to `responseBody`.
- Preserve the existing local-agent request, response, and error contracts.
- Do not change the challenge API, authentication flow, UI, or caching behavior.

Add a regression check that extracts the main inline JavaScript from
`web/index.html` and runs a JavaScript syntax parser against it. The check must
fail on duplicate lexical declarations and other parse-time errors before a
release is published. It may skip only when the parser runtime is unavailable,
while the release verification path must run it explicitly.

## Verification

1. Run the web JavaScript syntax check.
2. Run `go test ./... -count=1`.
3. Deploy the corrected web bundle.
4. Open `https://cm.hsgitlab.xyz/` in Chrome.
5. Verify the challenge text changes from `校验题加载中...` to a generated
   arithmetic question.
6. Verify the browser console has no JavaScript syntax errors.
7. Verify `GET /api/auth/challenge` still returns `200` with `question` and
   `token`.

## Rollback

The change is isolated to a variable rename and its regression check. Rollback
is a normal package downgrade to the previous version; no data or configuration
migration is involved.
