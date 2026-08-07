# `cm skill diff` Design

## Goal

Add a read-only command that shows how the installed `connectmac` skill differs from the skill embedded in the current `cm` binary. This lets users review local changes before deciding whether to run `cm skill update --force`.

## Command

```text
cm skill diff [--skills-dir <path>]
```

The command uses the same default skill directory and `--skills-dir` parsing as the other `cm skill` commands.

## Compared Files

The command compares these managed files independently:

- `SKILL.md`
- `agents/openai.yaml`

The installed file is the old side of the diff and the current built-in template is the new side. Diff headers identify both the relative file and its source, for example:

```diff
--- installed/SKILL.md
+++ built-in/SKILL.md
```

## Behavior

- When both files match, print `connectmac skill matches the built-in version` and exit `0`.
- When one or both files differ, print a unified diff for each changed file and exit `0`.
- When the skill is not installed, print an actionable error and exit `1`.
- When a managed file cannot be read, print the file-specific error and exit `1`.
- Do not read or modify project rule files, skill state, backups, or unrelated files.
- Do not write any files.

Differences are successful diagnostic output rather than command failure, so shell scripts can distinguish operational errors from an ordinary modified skill.

## Implementation

Add a `SkillManager.Diff()` operation that returns the rendered text and whether differences exist. Generate unified line diffs inside Go so the command does not depend on a system `diff` binary. Keep the diff implementation scoped to the two small text files managed by ConnectMac.

Wire the operation through `runSkill`, usage text, shell completion, README examples, and first-use guidance where applicable.

## Tests

Cover:

- missing skill returns an error;
- current skill reports no differences;
- modified `SKILL.md` emits a unified diff;
- modified metadata emits a separate unified diff;
- custom `--skills-dir` resolves correctly;
- command does not alter installed files or state;
- Bash, Zsh, and Fish completion expose `diff`.

Run `gofmt`, `go vet ./...`, `go test ./... -count=1`, and shell completion syntax checks before completion.
