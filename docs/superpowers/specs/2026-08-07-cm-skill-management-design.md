# CM Skill Management Design

## Summary

ConnectMac will replace the public `cm init-rules` command with a focused `cm skill` command group. The command group manages only the built-in `connectmac` skill and its associated AI rule installation. It is not a general third-party skill manager.

The replacement happens in the current release. `init-rules` will be removed from command dispatch, help, completion, guides, tests, and documentation without a compatibility alias.

## Goals

- Give users explicit install, inspect, update, validate, and uninstall operations for the built-in skill.
- Protect locally edited skill files from accidental overwrite.
- Keep Homebrew and APT package upgrades separate from user-owned skill updates.
- Preserve the existing four-agent rule installation behavior for Codex, Claude, Trae, and Cursor.
- Make removal conservative: delete only ConnectMac-owned files or marked rule blocks.

## Non-Goals

- Installing or managing third-party skills.
- Discovering skills from a registry.
- Automatically updating files under a user's home directory during package upgrade.
- Changing AWS, MCP, Web, profile, or SSH behavior.
- Migrating arbitrary manually installed skill layouts.

## Command Interface

```text
cm skill setup [--agent <codex|claude|trae|cursor>] [--project <path>] [--skills-dir <path>] [--dry-run]
cm skill install [--skills-dir <path>] [--dry-run]
cm skill status [--skills-dir <path>]
cm skill update [--skills-dir <path>] [--force] [--dry-run]
cm skill validate [--skills-dir <path>] [--agent <agent>] [--project <path>]
cm skill path [--skills-dir <path>]
cm skill print
cm skill uninstall [--skills-dir <path>] [--rules --agent <agent> --project <path>] [--dry-run]
```

### `setup`

`setup` is the replacement for `init-rules`. It:

1. Resolves the target Agent. If `--agent` is absent and stdin is interactive, it prompts the user. Non-interactive use without an Agent fails.
2. Defaults `--project` to the current directory.
3. Writes the canonical rule source to `~/.connectmac/rules.md`.
4. Upserts the ConnectMac marker block into the selected Agent rule file.
5. Installs the built-in skill.
6. Writes managed installation state.
7. Validates the completed installation.

Agent rule paths remain unchanged:

- Codex: `<project>/AGENTS.md`
- Claude: `<project>/CLAUDE.md`
- Trae: `<project>/AGENTS.md`
- Cursor: `<project>/.cursor/rules/connectmac.mdc`

### Skill-only Commands

- `install` installs the embedded skill without changing project rules.
- `status` compares installed files with embedded templates and managed state.
- `update` updates an unmodified managed installation.
- `update --force` backs up a modified installation and then replaces it.
- `validate` checks required files, frontmatter, metadata, managed state, and optional project rules.
- `path` prints the resolved skill directory only, making it script-friendly.
- `print` writes the embedded `SKILL.md` to stdout and does not modify files.
- `uninstall` removes the managed skill installation but preserves project rules by default.

### Rule Removal

`uninstall --rules` additionally removes only the ConnectMac marker block from the requested Agent rule file. Both `--agent` and `--project` are required with `--rules` so removal never guesses a project or target file.

If the rule file contains unrelated content, that content remains byte-for-byte except for surrounding blank-line normalization at the removed block. If a Cursor rule file becomes empty after removing the managed block, the empty file may be removed. Parent directories are not recursively deleted.

## Storage and Ownership

The default skill path remains:

```text
~/.agents/skills/connectmac
```

The installed skill contains only standard skill content:

```text
SKILL.md
agents/openai.yaml
```

Managed state is stored outside the skill directory:

```text
~/.connectmac/skill-state.json
```

The state schema contains:

```json
{
  "schema_version": 1,
  "skill_name": "connectmac",
  "skill_path": "/absolute/path/to/connectmac",
  "cm_version": "0.1.x",
  "skill_sha256": "...",
  "openai_yaml_sha256": "...",
  "installed_at": "RFC3339 timestamp",
  "updated_at": "RFC3339 timestamp"
}
```

Hashes are calculated from the exact embedded templates written by the running binary. State files use mode `0600`; skill files use `0644`; directories use `0755` except the existing private `~/.connectmac` directory, which remains `0700`.

## Status Model

`cm skill status` reports one of five stable states:

- `missing`: the skill directory or required files do not exist.
- `current`: installed files match the running binary's embedded templates.
- `outdated`: installed files match their recorded managed hashes but differ from the running binary's templates.
- `modified`: installed files differ from both recorded managed hashes and current templates.
- `invalid`: required files or state are unreadable, malformed, or internally inconsistent.

The command also prints the skill path, installed CM version when known, current CM version, and a suggested next command. Status exits `0` for `current`, `missing`, and `outdated`, and exits `1` for `modified` or `invalid`. A missing installation is an inspectable state rather than a command failure.

When legacy skill files exist without managed state:

- If both files exactly match current embedded templates, `status` reports `current` and `install` may adopt them by writing state.
- Otherwise `status` reports `modified`; only `update --force` may replace them.

## Update and Backup Safety

Normal `update` is allowed only for `current` or `outdated` managed installations. It refuses `modified` and `invalid` states.

`update --force` creates a timestamped backup before writing:

```text
~/.connectmac/backups/skills/connectmac-YYYYMMDD-HHMMSS/
```

The backup contains the existing skill directory and state file when present. If backup creation or verification fails, update stops without changing the installation. Writes use temporary files followed by rename so partial updates do not leave truncated skill files.

`install` refuses to overwrite a modified existing installation and directs the user to `cm skill update --force`.

## Integration with `cm init`

The existing `cm init` and `cm init wizard` prompt remains. Accepting the prompt invokes the same internal setup service as `cm skill setup`, including Agent selection, installation state, and validation. User-facing output refers to `cm skill setup`, never `init-rules`.

## Help and Completion

Top-level usage and completion add `skill` and remove `init-rules`. A new completion target exposes:

```text
setup install status update validate path print uninstall
```

Zsh, Bash, and Fish completion route `cm skill <TAB>` through this target. Options are documented in command help and README examples.

## Error Handling

- Unknown command or option: exit `2` with concise usage.
- Filesystem, backup, parsing, or validation failure: exit `1` with the affected path and operation.
- Modified installation without `--force`: exit `1` and print the safe recovery command.
- Unsupported or missing non-interactive Agent: exit `1`.
- Dry-run operations print all target paths and intended actions and write nothing.
- Uninstall of an already missing skill succeeds and reports that no managed skill was present.

No error message prints skill content, tokens, credentials, PEM paths, or unrelated rule-file content.

## Documentation Migration

The README and first-use guide use `cm skill setup --agent codex --project .`. Documentation explains that package upgrades do not update user-installed skills and recommends:

```text
cm skill status
cm skill update
```

All public references to `cm init-rules` are removed. Historical design and release documents are not rewritten.

## Testing

Table-driven and focused tests cover:

- Dispatch, usage, and exit codes for every subcommand.
- Fresh install and setup for all four Agent paths.
- Idempotent install and setup.
- Status transitions: missing, current, outdated, modified, and invalid.
- Adoption of a matching legacy installation without state.
- Refusal to overwrite modified files.
- Force-update backup creation and preservation of prior content.
- Backup failure leaving the original installation unchanged.
- Skill-only uninstall preserving project rules.
- `--rules` removal preserving unrelated rule-file content.
- Dry-run performing no writes.
- Top-level and shell completion exposing `skill` and removing `init-rules`.
- README/guide-facing command examples where tested as generated output.

The full Go test suite must pass after migration.

## Acceptance Criteria

- `cm init-rules` is no longer a recognized command.
- Every documented `cm skill` command behaves as specified.
- Existing installed skill content is never silently overwritten when modified.
- Homebrew/APT upgrades do not modify user skill files.
- `cm init` can still initialize AI rules and the skill through the new implementation.
- Agent rule removal cannot delete unrelated project instructions.
- Shell completion and user documentation contain no active `init-rules` references.
