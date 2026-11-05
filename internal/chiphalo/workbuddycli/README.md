# WorkBuddy official-client adapter

## Ciyun native Intl routing

With `--native-url http://127.0.0.1:18336`, the DeepSeek Intl model is forwarded
unchanged to the isolated CangShui Go gateway. This path bypasses both the CLI
process and its single-worker queue, and permits concurrent upstream streams.
The original management proxy and explicitly allowlisted legacy models remain
on the original plugin endpoint; the public worker stays on port 8332.

The pinned CangShui base is `bd34ba70cd13284f1d9baaf616015c5e3e05f965`.
Its CPA deployment mode reads the existing access-only projection on every
request, verifies the account UID and expiry, and never refreshes or persists
credentials. Inference has no wall-clock deadlines. Caller instructions are not
subject to the upstream project's blocked-template substitutions. Each request
uses an immutable credential snapshot instead of holding an account lock while
waiting for upstream response headers. CPA remains the billing/receipt owner.

Without `--native-url`, the official CLI behavior documented below is retained
as an explicit rollback path, not an automatic retry after an ambiguous failure.

The Go adapter runs an **unmodified, isolated official WorkBuddy CLI** over ACP
stdio. It uses the documented `CODEBUDDY_AUTH_TOKEN` authentication mechanism;
it does not implement client attestation or reproduce private channel signatures.

## Supported contract

- `deepseek-v4.1-flash`, text input, default/high reasoning.
- Streaming and non-streaming Chat Completions. The existing CPA translator
  provides Responses API compatibility, including custom tools.
- Explicit ordinary-text `response_format` defaults from Codex/Responses clients;
  structured JSON output constraints remain unsupported and are rejected.
- Declared function proposals, parallel proposals, and actual downstream tool
  result history. No tools are executed inside this adapter.
- Final upstream token usage, including cached and hidden reasoning tokens.
  Existing CPA token pricing is not replaced by upstream credit balances.
- `max_tokens` / `max_completion_tokens` (or Responses `max_output_tokens`)
  from 1 to 100000, passed to the official CLI through its per-request
  `CODEBUDDY_CODE_MAX_OUTPUT_TOKENS` environment. Conflicting aliases are rejected;
  omitted/null limits retain the official default. ACP `max_tokens` completion
  maps to `finish_reason: length`; incomplete tool proposals still fail closed.

Unsupported modalities, external reasoning history, and generation controls are
rejected explicitly rather than silently removed. The exact translator marker
`[reasoning unavailable]` is allowed as missing reasoning, not actual reasoning.
ACP history represents matched call/result pairs; it does not preserve original
parallel batch grouping, although every result remains paired with its call.

## Isolation and credentials

The legacy plugin is the only refresh owner. A separate Go exporter projects
only the current access token, account UID, and expiry to a protected runtime
directory. The CLI UID cannot read the original credential or refresh token.
Every new request verifies the configured account binding and projection expiry.

Every request uses a private ephemeral home. The explicit environment excludes
ambient credentials, proxies, API overrides, hooks and MCP settings. Plugin
installation and session-title generation are disabled. Only `DelegateTool` is
available; all client-side execution RPCs and permission requests are denied.
Standard ACP cancellation settles the official CLI's parallel approval queue.
Incomplete or undeclared proposals fail closed. No artificial success or token
usage is emitted after an upstream failure. Cancellation kills the child process
group and removes its request directory. Established inference has no wall-clock
network deadline.

The adapter has one active infrastructure resource slot and a cancellation-aware
waiting queue (default 8 requests, at most 32 MiB of queued raw payloads).
Streaming waiters receive SSE comment heartbeats, never invented model tokens.
Only a full queue returns HTTP 429 `worker_queue_full`; an occupied worker alone
does not reject overlapping requests. `--max-queued=0` restores fail-fast behavior.
Waiting and established inference have no artificial wall-clock deadlines.
An explicitly configured legacy-model allowlist can be proxied unchanged without
using that slot. A fixed loopback management proxy retains the original worker's
authentication; downstream API keys never become management keys.

## Deployment

Use a loopback-only listener, separate service UID, immutable official runtime,
read-only token projection and API-key file, private writable home, no capabilities,
and systemd memory/task limits. The validated dependency is the **full official
launcher** from `@tencent-ai/codebuddy-code` 2.161.0 with esbuild 0.25.12; the lite
entrypoint does not implement the required ACP agent. Pin the dependency lockfile.

On Ciyun, the existing public worker endpoint remains `127.0.0.1:8332`; the
original Intl plugin runs behind it at `127.0.0.1:18332` and retains refresh and
quota ownership. CN, main gateway/GUI binaries and the origin allowlist are not
changed. Only the new Intl model is enabled; existing disabled rows stay disabled.

Production rollout and rollback use the existing Go `apply_bundle` transaction
helper. A read-only Go scope guard confirms that published configuration, model
catalog and context outside Intl do not change. State is reconstructed from the
**current published configuration**, so unrelated GUI drafts are not applied.
Rollback disables the new model and restores the old worker listener; it never
restores historical usage, keys, credentials or provider-state snapshots.

Validated with unit/race tests and real Linux/public CPA Responses calls: text
SSE, function handoff, actual result continuation, custom raw-input tools,
parallel handoff and reordered result continuation. Never test Banked Reset.
