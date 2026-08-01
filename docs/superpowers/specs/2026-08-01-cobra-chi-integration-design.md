# Cobra and chi Incremental Integration Design

## Status

Approved for implementation planning on 2026-08-01.

## Objective

Incrementally introduce Cobra for the `cm` command tree and chi for HTTP routing while preserving all existing command-line and HTTP behavior. The migration must not change business rules, AWS safety guarantees, user workflows, public output, or deployment behavior.

Echo and Fiber are explicitly out of scope. The target framework set is:

- CLI: Cobra
- HTTP routing and middleware: chi plus the Go `net/http` standard library
- WebSocket: Gorilla WebSocket
- AWS: AWS SDK for Go v2
- Database: `database/sql`

## Compatibility Contract

### CLI

The migration must preserve:

- Command and subcommand names
- Positional arguments and flags
- Default values
- Exit codes
- Script-relevant stdout and stderr text
- Help behavior where external tests or automation depend on it
- Shell completion behavior
- MCP stdio startup and tool behavior

Cobra must be configured with `SilenceErrors` and `SilenceUsage` so it does not add error prefixes or usage output that would alter existing failure behavior. Command suggestions and other automatic output must remain disabled unless compatibility tests prove that they do not change observable behavior.

### HTTP

The migration must preserve:

- URL paths and query parameters
- HTTP methods
- Request and response JSON fields
- HTTP status codes
- Cookie names, attributes, and session behavior
- Request ID behavior
- Authentication and authorization semantics
- CORS and security-header behavior
- Local Agent TLS behavior
- WebSocket and streaming behavior

The current frontend, Local Agent, CLI remote-profile client, MCP server, and existing API consumers must continue working without protocol changes.

## Architecture

### Cobra Layer

The Cobra layer is a thin command adapter. It owns:

- Command hierarchy
- Argument and flag parsing
- Help and completion registration
- Invocation of existing application functions
- Mapping results to the existing exit-code contract

It must not contain AWS, SSH, VNC, transfer, configuration, persistence, or notification business logic.

The initial command tree is organized around the existing public surface:

```text
cm
├── aws
├── profile
├── local-agent
├── member
├── job
├── mcp
├── web
├── connection and transfer commands
└── legacy compatibility adapter
```

Unmigrated commands continue through a legacy adapter that delegates to the existing `App.Run` path. Each command moves to a native Cobra command only after compatibility tests cover its arguments, output, and exit status.

### chi Layer

chi becomes the single HTTP router. Echo is not introduced.

The chi layer owns:

- Route grouping
- HTTP method matching
- Middleware composition
- Authentication and role middleware attachment
- Invocation of existing `http.Handler` and `http.HandlerFunc` implementations

It does not own business logic or response-schema formatting. Existing handlers remain responsible for their current JSON and status-code contracts.

The intended route structure is:

```text
chi.Router
├── common middleware
│   ├── request ID
│   ├── runtime and audit logging
│   ├── panic recovery
│   └── security headers
├── /api/auth
├── /api/profiles
├── /api/members
├── /api/jobs
├── /api/aws
├── /api/local-agent
└── existing static, WebSocket, and compatibility handlers
```

During the first chi phase, the router mounts existing handlers unchanged. Route modules and middleware are extracted only after the compatibility router passes the existing API suite.

### Business Layer

Existing application and service code continues to own:

- AWS Mac create, open, readiness, and destroy safety rules
- Dedicated Host reuse and lifecycle behavior
- Elastic IP retention
- Profile and member management
- Background jobs and lifecycle coordination
- Release reminders and Enterprise WeChat notifications
- SSH, known-host, VNC, transfer, and Local Agent behavior
- MySQL persistence, logs, and audit events

Framework migration must not be used as an opportunity for unrelated business refactoring.

## Migration Sequence

### Phase 1: Compatibility Baseline

Add table-driven and snapshot-style tests for the existing CLI and API surface before changing entry points. The baseline records command arguments, flags, exit codes, relevant output, API methods, paths, status codes, JSON envelopes, Cookie behavior, and middleware-visible headers.

### Phase 2: Cobra Root and Compatibility Adapter

Introduce a Cobra root command with native `version` and `completion` commands. All other commands initially delegate to the existing dispatch path. Confirm that Homebrew completion generation, MCP startup, scripts, and tests remain unchanged.

### Phase 3: Incremental Cobra Commands

Migrate in this order:

1. Profile and read-only utility commands
2. SSH, VNC, start, stop, push, pull, and known-host commands
3. Local Agent commands
4. Member and job commands
5. Web and MCP startup commands
6. AWS commands

AWS commands move last because they have the strictest safety and preview-confirm contracts. No AWS command migration is accepted unless all existing mutation-safety tests pass unchanged.

### Phase 4: chi Compatibility Router

Create a chi router that mounts the current handlers with the same paths and methods. Keep existing middleware behavior intact and prove the complete API contract before reorganizing routes.

### Phase 5: chi Route Modules and Middleware

Move route groups in this order:

1. Authentication
2. Profiles
3. Members
4. Jobs and lifecycle status
5. AWS
6. Local Agent and WebSocket routes

Extract request ID, logging, recovery, security headers, and role enforcement into composable middleware without changing their observable responses.

### Phase 6: Legacy Removal

Remove the old CLI switch dispatch and old `http.ServeMux` registration only after every command and route has a native replacement and all compatibility tests pass. Business functions remain independent of Cobra and chi.

## Error Handling

Cobra delegates business errors to the existing formatting and exit-code behavior. It must not automatically print usage for runtime failures.

chi does not rewrite errors returned by existing handlers. Recovery middleware handles only otherwise unhandled panics, records the request ID, and emits the existing JSON 500 envelope. Authentication, authorization, validation, conflict, not-found, and AWS errors retain their existing status codes and payloads.

Logs and panic reports must redact:

- Passwords
- Session Cookies
- API and Bearer tokens
- Webhook keys
- PEM paths when they identify sensitive local configuration
- AWS access keys and secrets

## Testing and Acceptance

Every migration step must pass:

```sh
go test ./...
node scripts/check-web-js.mjs
node scripts/check-web-workbench.mjs
npm run check:web:visual
```

Additional migration-specific coverage includes:

- CLI argument, flag, output, and exit-code compatibility tests
- Shell completion compatibility tests
- MCP initialization and tool-list tests
- API method, path, JSON, status, Cookie, and header contract tests
- WebSocket and Local Agent TLS tests
- AWS preview-confirm and EIP-retention tests
- Homebrew formula tests
- APT package smoke tests

The migration is complete only when existing user commands, web workflows, MCP configuration, Homebrew installation, APT installation, and staging deployment require no user-facing changes.

## Delivery and Rollback

Each phase is delivered as small, independently testable commits. Cobra and chi compatibility adapters remain in place until the corresponding migration is complete. A failed phase is rolled back at the commit level without requiring configuration or database migration.

No schema migration, API version change, new service port, or AWS resource mutation is part of this framework integration.

