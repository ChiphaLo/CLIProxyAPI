# CPA compressed and incremental transport

This is the Go **candidate**, based on CLIProxyAPI v8.0.25. It does not replace
the running production Python gateway/GUI. `cmd/cpa-gateway` still refuses
general production startup: the remaining migration/security gates are not
removed by these transport features. The `--legacy-candidate` path runs only
against an explicitly marked private snapshot root on a loopback listener,
with credential refresh disabled. Its readiness contract deliberately cannot
activate a production release. Do not deploy it over the production service.

## Live-state compatibility validation

The candidate now connects the legacy identity and receipt adapters to the real
SDK lifecycle, rather than accepting only synthetic upstream credentials:

- Existing admin and SHA-256 issued-key identities are retained. The host pins
  its access provider on the manager itself, so plugin/config reconciliation
  cannot introduce a config-inline fallback around a disabled issued key.
- Fully validated key, account and exposed-model snapshots reload on new
  admissions. Malformed edits fail closed, and in-flight receipts retain the
  exact account identity selected at admission.
- Existing JSONL bytes and permissions are preserved. Appends reopen replaced
  files rather than retaining an obsolete inode; incomplete historical lines
  and unsafe destinations are rejected instead of being repaired or truncated.
- Global, per-IP and dynamically lowered per-key limits share bounded counters
  across identity reloads. Key limits run before compressed-body decoding.
- SDK watchers subscribe to the config parent directory and filter the exact
  filename, preserving repeated atomic GUI publication. Embedded config
  transforms constrain runtime hosting paths without writing source YAML or
  hashing a GUI-owned management secret back into that file.
- Admission and upstream-attempt failures append to the existing audit schema.
  Provider error bodies, authorization headers and query parameters are not
  copied into the shared audit log.
- Shadow validation disables scheduled, forced and 401-triggered OAuth refresh;
  a copied refresh token must not compete with the production token owner.

On 2026-10-13 the scoped Linux retention suites passed 97 test cases, including
a real candidate process against a 63-file private production snapshot on
hyvps. That process exposed the three current providers and 13 public models;
the running ciyun gateway independently exposed the same model set. The tests
exercised issue/disable/revoke behavior, both fixed OAuth catalogs, model-filter
edits, repeated provider removal/restoration and unchanged historical receipts.
They ran with no external network, production mounts, inference requests or
credential refresh. Windows focused tests, gateway/access race checks, scoped
vet and the required `cmd/server` build also passed. This is not a full-product
or full-repository certification.

The production GUI/helper management contract, token-owner handoff and stable
blue/green listener bootstrap remain deployment gates. A production rollback
must reselect code/routing only, never overwrite shared business state with an
older snapshot. No production activation or Banked Reset was performed.

## Reference implementation review

Reviewed [xz-dev/AI-gateway](https://github.com/xz-dev/AI-gateway/tree/12dd903b342e1ea93965ffb58892a4d4eaea40e6)
and its `ai-sse-keepalive-proxy` submodule at
`bd38e50f9fc09e1c446d3573b31cd54ecec583df`:

- APISIX enables WebSocket proxying and disables response buffering on streaming
  routes. This is not, by itself, incremental context upload.
- The keepalive proxy negotiates `permessage-deflate` independently on each WS
  leg and streams application messages through bounded buffers. That reference
  uses compression context takeover and delegates message limits to its backend.
- Its eligible HTTP SSE path requests identity encoding, disables automatic
  decompression, and rejects compressed late responses. The reviewed repository
  does not implement universal HTTP gzip/Brotli/zstd on both legs.
- Its README explicitly distinguishes `websocket` (full context) from
  `websocket-cached` (`previous_response_id` plus new input), and says the latter
  is not supported on its current CPA-backed path.

No reference implementation code was copied. SSE output deltas, compression,
and incremental input uploads are three independent features.

## Capability matrix

| Leg / mode | Candidate behavior |
| --- | --- |
| Client -> CPA HTTP | Accept gzip, br, zstd `Content-Encoding`; bound both encoded and decoded input to 64 MiB |
| CPA -> client HTTP | Negotiate gzip/br/zstd with `Accept-Encoding` quality/exclusion values; encode successful API JSON/SSE only |
| CPA -> provider HTTP upload | Identity by default; opt in independently per reviewed credential, above 1 KiB |
| Provider -> CPA HTTP | Keep stock Go automatic gzip by default; opt-in gzip/br/zstd negotiation and streaming decoding |
| Client -> CPA HTTP continuation | `previous_response_id` plus only new input; bounded local replay when upstream lacks native state |
| Client <-> CPA WS | RFC 6455 with negotiated `permessage-deflate` without context takeover |
| CPA <-> Codex native WS | Reuse upstream v8's compressed, account-pinned continuation; upload only new input when the selected credential enables WS |
| WS -> HTTP-only provider | Accept client delta; reconstruct full provider context; no claim of provider-side incremental savings |

The CPA profile exposes GET/upgrade and POST on `/v1/responses` and
`/backend-api/codex/responses`. Other management/realtime surfaces remain closed.
It supports ordinary `response.create` turns and `response.interrupt`, **not**
multi-lane `stream_id`, steering, or arbitrary control events. Client/proxy WS
support must be enabled separately; a gateway cannot make an unchanged client
stop uploading full context.

## HTTP configuration

Install `traffic.HTTP()` after authentication and before `gateway.Admission`.
`LegacyProfile.ServerOptions()` already does this. Do not install it on GUI,
management, authentication, or cookie/secret-bearing HTML routes.

Example v8 provider entry (no real credential):

```yaml
api-keys:
  openai-compatibility:
    - name: reviewed-provider
      prefix: example
      base-url: https://provider.example/v1
      keys:
        - api-key: REPLACE_PRIVATELY
          request-compression: gzip
          response-compression: zstd
```

Omit both compression fields until capability is verified. Allowed values are
empty, `identity`, `gzip`, `br`, and `zstd`. Request support cannot be inferred
from a provider's compressed responses. OAuth runtime metadata may use trusted
`request_compression` / `response_compression` fields; this change does not edit
any stored production OAuth credential. Existing explicit `Accept-Encoding`
headers remain authoritative.

Compression is applied by the HTTP transport **after** the final business payload
rules, and decompression happens before translation/usage parsing. A failed
compressed POST is not automatically resent as identity by this transport.
Existing independently configured SDK/provider retry policies are not changed.

Downstream SSE compression flushes the codec and HTTP writer on each handler
flush; it does not buffer a whole response. `Vary: Accept-Encoding` is preserved.
Errors, existing encoded responses, `Set-Cookie`, `no-transform`, and non-API
content are not compressed. Unsupported request encodings return 415; malformed
compressed input returns 400; oversized decoded input returns 413; refusing all
available response encodings returns 406.

## Incremental clients and safe recovery

First request: send the full input. After a completed response, send the same
public model and the returned response ID, with only new items:

```json
{
  "model": "example/model",
  "previous_response_id": "resp_previous",
  "input": [{"type": "function_call_output", "call_id": "call_previous", "output": "42"}],
  "stream": true
}
```

For HTTP, resupply the current tools/instructions/options. Only prior input and
completed output are carried forward, not the previous request's settings.
The local HTTP store is scoped by public key ID **and credential hash**, exact
public model, endpoint, and selected auth ID. It is volatile, has a 15-minute
lookup TTL, at most 128 entries / 32 MiB of stored input, and a 4 MiB entry/event
limit. Output from failed, incomplete, canceled or truncated responses is
not committed as usable continuation state. No content is written to disk by
this cache. A restart, expiry, oversized entry, eviction or ownership mismatch
returns 409: resend the full context explicitly without `previous_response_id`.
Do not blindly retry a delta after changing model, Key or OAuth account.

Native WS state is connection/account scoped. Each CPA frame is checked, each
turn is authorized and attributed independently, and account guards still run
at execution time. The reader bounds decompressed messages to 64 MiB, uses a
one-item queue, and admission caps the profile at eight active WS connections.
Foreign browser origins and cross-account continuation are rejected. Open a
new connection and replay full context after losing upstream state.

Sending fewer bytes does **not** remove historical input from the model's
context or billing. The existing raw-usage CPA price calculations remain intact,
including output tokens containing hidden reasoning. See official OpenAI
[conversation state](https://developers.openai.com/api/docs/guides/conversation-state)
and [WebSocket continuation](https://developers.openai.com/api/docs/guides/websocket-mode).

## Verification

Tests cover real loopback HTTP/SSE/WS connections, three codecs, early SSE flush,
bounded decompression, cache expiry/eviction, tool-result continuation, native WS
delta upload, full replay to HTTP-only providers, and identity/Origin/policy
negative cases. Run:

```sh
go test ./internal/chiphalo/traffic ./internal/chiphalo/gateway
go test ./internal/config ./internal/watcher/synthesizer -run TestRequestCompression
go test ./sdk/api/handlers/openai -run 'TestResponsesWebsocket|TestNormalizeResponsesWebsocket|TestNormalizeSubsequentRequest|TestForwardResponsesWebsocket'
go build ./cmd/server
```

The restricted `cmd/cpa-traffic-probe` and `cmd/cpa-live-broker` support a bounded
two-turn real-provider test with a deterministic locally executed tool. They
are test helpers, not production listeners, and do not expose credentials to
the test client. No Banked Reset operation is involved.

### Measured candidate validation (2026-10-13)

On the isolated hyvps Linux runner, the final scoped transport, gateway,
configuration, synthesizer, HTTP helper and Responses WS suites passed 231
test cases with no failures. Windows focused tests, traffic/gateway race checks,
scoped `go vet`, and the required `cmd/server` build also passed.

Two bounded real-provider runs used `1x/gpt-6.1-sol`. Each executed a declared
`lookup(7)` tool locally and continued with only the tool result and response ID;
both turns returned HTTP 200 and the final answer was verified as `42`.
The final run measured application-body bytes (excluding headers/TLS):

| Request / response | Identity bytes | Compressed wire bytes |
| --- | ---: | ---: |
| First full input upload | 17,314 | 489 |
| Second incremental upload | 450 | 325 |
| First SSE download | 4,982 | 1,908 |
| Second SSE download | 3,152 | 1,280 |

The real provider accepted gzip request bodies. This provider was HTTP-only,
so CPA replayed the complete tool conversation upstream; its second upstream
input usage was 3,719 tokens, not the token count of the delta alone. A separate
native Codex WS loopback test verified upstream delta transmission: the first
upstream message was 120,346 bytes and the second was 365 bytes containing only
the new tool result and continuation ID. This is a protocol fixture, **not** a
live official OAuth WS capability test. These measurements are samples, not
guarantees for all models/providers.

The wider upstream suite's `TestResponsesInterruptStopsHTTPUpstream` cancellation
assertion also failed on the unmodified baseline in repeated runs. No unrelated
interrupt behavior was changed, and the scoped WS regression suite passed;
these results must not be represented as a completely green full-repository run.
No production service or stored OAuth credential was replaced by this validation.

## OpenCode Muse native Responses

The selected credential's configured `opencode` prefix and a `muse-spark-*`
upstream model enable the dedicated `/responses` path. A caller-supplied model
prefix alone cannot enable it. Other compatible providers retain their Chat
Completions path; the upstream Meta executor is unchanged.

The Go adapter promotes trusted developer/system `additional_tools` to top-level
declarations and removes those records from `input`. Namespace/custom tools are
represented as native functions upstream, then restored on JSON responses and
SSE events, including tool history and raw custom input. All ordinary protocol
conversion still uses the upstream SDK. Native argument/schema errors,
undeclared calls, conflicting identities and truncated SSE fail explicitly.
The gateway itself never evaluates or executes a tool.

Muse accepts only `auto` tool selection. Client `none` is implemented by removing
the tools before the final payload rules, not by silently permitting calls.
`required` and named choices are rejected explicitly. Final payload overrides
remain authoritative: unsupported final choices/declarations are rejected, never
rewritten after the final rules. This adapter supports function/custom/namespace
tools, not built-in provider tools or `/responses/compact`.

Portable summaries from another model retain their contents but lose an unknown
reasoning item ID that Muse cannot resolve. Known successful native reasoning IDs
remain intact and are scoped by source credential, downstream caller, exact model
and session. The volatile provenance store contains at most 4,096 IDs for 15
minutes, not reasoning content. Foreign opaque encrypted reasoning is rejected;
the adapter does not decrypt it or silently discard it. HTTP continuation uses
the existing account-pinned CPA context replay, not an unscoped provider ID.

### Real Muse validation

On 2026-10-13 the isolated Go candidate passed four actual
`opencode/muse-spark-1.3-contributor` requests: JSON, SSE text, namespace/custom
`lookup` and its incremental tool-result continuation. All returned 200 and the
tool result was verified as `42`. Raw usage matched all four CPA receipts.
The tool request's 258 output tokens already included 190 reasoning tokens;
they were not added a second time.

A separate real **Codex CLI 0.162.1** run executed `exec_command`, the custom
`apply_patch` tool and `exec_command` again inside a read-only, network-isolated
container with a sole scratch-directory write mount. The output file exactly
matched a random challenge plus `CODEX_TOOL_ROUNDTRIP_OK`. Its four requests
completed successfully, and CPA receipt totals matched Codex: 13,793 input,
9,811 cached input and 350 output tokens (including 72 reasoning tokens).

Direct test-host egress was rejected by Muse's regional policy. Validation used
the existing approved CPA SOCKS egress through a temporary pinned SSH tunnel;
no production gateway, proxy, SSH or firewall configuration was changed. A
separate initial 256-token probe correctly returned `response.incomplete` when
its output budget was exhausted; the successful probe supplied an adequate
explicit budget. These are test limits, not new production defaults.

## Release ingress and OAuth ownership primitives

The isolation switchboard accepts an explicit `--trusted-ingress` list of exact
loopback IP addresses. The default trusts no forwarding metadata. This opt-in
is for a stable local ingress such as Caddy, not a public proxy allowlist.
It validates a bounded `X-Forwarded-For` chain and uses the nearest untrusted
hop, preventing a spoofed value to its left from becoming the client identity.
It forwards only the canonical address, preserves an ingress-reported HTTP/HTTPS
scheme, and drops alternate client-IP headers. Malformed trusted metadata is
rejected before reaching a selected release. A downstream Go listener must
independently configure its trust in the switchboard's exact loopback peer;
canonical headers alone do not make `gin.ClientIP()` trust them.

`internal/chiphalo/refreshowner` supplies a Linux process-lifetime, private flock
lease. Acquire it **before** loading shared OAuth files. Install its `Begin`
method using `Manager.SetCredentialRefreshGuard` before registering credentials.
The guard spans the complete refresh and persistence attempt, including a
successful rotation whose request context was canceled. Ordinary SDK runtimes
without a guard retain upstream behavior, and shadow refresh remains disabled.

Draining refuses new refreshes and waits for admitted refresh-and-persist
transactions before releasing ownership. Canceling that wait keeps the lease
held; a new owner cannot proceed. The lock inode is never removed on handoff.
Replacement or unsafe permission changes fail closed. A drained process cannot
resume with its old in-memory credentials: the next owner must restart and read
the current shared files, never a release-local auth snapshot.

These are **not yet a production token-owner handoff**. The legacy Docker
runtime does not participate in this flock protocol and must be explicitly
quiesced and verified exited before a new owner may refresh the same tokens.
The upstream SDK also intentionally treats ordinary OAuth persistence failures
as non-fatal warnings. A successful refresh return therefore does not prove
durability. Production hosting still needs a persistence-health gate and must
not hand off, restart or restore stale credentials after an unconfirmed save.
The guard does not change that upstream persistence policy.

### Scoped Linux verification

On 2026-10-13 the hyvps network-isolated runner passed **99 tests/subcases with
0 failures and 0 skips**: refresh lease 4, release 41, and SDK refresh/persistence
regressions 54. It ran as UID 65534 with all capabilities dropped. An initial
UID-0 run skipped the permission-fault test; the unprivileged rerun exercised it
successfully rather than weakening or deleting the test.

The actual `cpa-release` executable was tested for trusted ingress, activation,
one-command rollback, restart, and committed state after SIGKILL. Lease tests
covered competing processes, kernel lock release after process exit, canceled
drain, idempotent transaction release, and replaced/unsafe lock paths. SDK tests
covered scheduled, forced and 401-triggered ownership denial and holding the
guard across persistence after cancellation, including upstream's non-fatal
save-error behavior. Windows focused race checks, scoped vet and the required
`cmd/server` build passed. No production data was mounted, and no inference,
OAuth refresh or Banked Reset request was sent.

Read-only production checks confirmed unchanged gateway, GUI and helper PIDs.
The current helper still targets legacy port 8318 and `/v0/management`; v8's
equivalent private routes are `/v8/management/credentials` and
`/v8/management/requests/api-call`, not `/v1/management`. The old status and
management assumptions, shared-file save health, persistent configuration
application and final Go GUI/helper/quota hosting are outstanding production
integration work. Both candidate and switchboard retain their isolation-only
gates. Passing these tests is not authorization to label the complete product
production-ready or to replace ciyun's live CPA.

## Catalog readiness and durable OAuth persistence

Shadow readiness now returns HTTP 503 until the actual SDK has registered the
enabled OAuth members and their catalogs, and the current config-synthesized
credentials and declared OpenAI-compatible routes. It uses upstream's stable ID
synthesizer, not a second hashing algorithm. A rotated config key, changed model
mapping/policy, or removed-but-still-loaded config credential cannot reuse the
previous readiness. Disabled OAuth accounts do not block unrelated providers.
An invalid current YAML also cannot advertise readiness while the SDK retains
its previous valid runtime config. Responses expose counts, not auth IDs or keys.

`internal/chiphalo/durableauth` is an opt-in host wrapper for Codex persistence.
It delegates the JSON merge and refresh-patch semantics to the upstream store.
Before mutation it arms and syncs a private marker shared across releases; after
the store returns it validates the resulting credential fields, restores the
existing owner/group/mode, and syncs the file and parent directory under the
upstream Codex inter-process file lock. Only confirmed persistence clears and
syncs the marker. Save failures, mismatched read-back or unexpected returned
paths leave sticky failure state and a restart fence; backend errors are not
echoed with credential material. A fence must not be automatically deleted by
deployment or rollback scripts. Failed marker-directory sync is an uncertain
filesystem state, not permission to force a handoff.

The upstream SDK's default non-fatal save-error contract remains unchanged.
A regression exercises that exact behavior: the manager returns success after
a failed save, but the wrapper reports unconfirmed health and refuses restart.
This protects persistence attempts, **not** a crash after upstream token rotation
but before `Save` begins. Full production hosting still needs the refresh-owner
transaction/drain protocol and must coordinate non-participating legacy writers.
The wrapper does not claim protection against arbitrary external writers that
ignore the shared ownership and Codex file-lock contracts.

The candidate wires this wrapper in **read-only** mode and continues to disable
all credential refresh. It cannot save, delete or redirect copied credentials.
An unconfirmed marker blocks readiness and new admissions. The writable wrapper
is tested with synthetic credentials and native Codex storage, not production
OAuth refresh. Account import/deletion transactions and non-Codex writable stores
are outside this wrapper's contract; it does not expose a generic file writer.

### Verification against the copied ciyun environment

The hyvps private CPA environment copy contains 12,912 files and a separate
pristine, working and shadow layout. Docker image identity was checked by the
portable config digest and all eight layer diff IDs: daemon manifest/config ID
differences alone do not establish a different image. The old binary's offline
embedded OAuth catalog differs from the running production catalog; the Go
candidate is compared to a captured live `/v1/models` reference instead.

After these changes, the copied HTTPS GUI/helper/legacy baseline plus Go candidate
passed 22 application checks, including real login/session handling, unchanged
provider application, bad-origin rejection, invalid-config readiness, persistence
fencing, and the 13-model production match. The Go/Linux focused suites passed
56 tests/subcases (durability 9, gateway 45, command 1, frozen real-command 1),
with no failures or skips. Windows focused race checks, scoped vet and the
required server build passed. Readiness cold-registration tests use deterministic
missing SDK registrations; they do not rely on observing a timing race in every
real process startup.

All runs used isolated networks and private data derivatives. No real inference,
OAuth refresh or Banked Reset request was sent, and production service PIDs and
tracked historical/config/key files were unchanged. The baseline's systemd and
Docker status reads were adapted to the isolated processes; these checks do not
establish host-level systemd sandbox equivalence. The Go runtime still has no
production launch mode. The private management/GUI/helper/quota integration and
actual refresh-owner handoff remain necessary before atomic production cutover.

## Incremental Go GUI control helper

`cmd/cpa-admin-helper` now supplies the existing newline-delimited Unix control
protocol for six local operations: issued-key create/update/delete/read and
provider-secret set/delete. It is **shadow-only** and requires an explicit
private copied-environment marker; it is not a production helper replacement.
The copied Python GUI uses its original IPC client and HTTP key routes, without
a Python subprocess fallback inside the Go helper. Unmigrated operations return
an explicit failure. The GUI itself and the quota collector are not yet Go.

Linux `SO_PEERCRED` restricts connections to the captured GUI UID. UID 0 does not
implicitly bypass that check. The socket retains root/GUI-group 0660 permissions
in a controlled 0750 directory. Envelopes reject unknown or duplicate fields,
invalid request IDs and missing newline framing. Request size is bounded to
1 MiB, response size to 64 KiB, and connections to 16 concurrent workers. An
oversized response returns complete error JSON rather than a truncated frame.
Only metadata is audited; request payloads and credentials are never logged.
Failure to persist the start audit rejects mutation before dispatch. If the
completion audit fails, the response explicitly reports an uncertain outcome.

Key mutations reuse the gateway's issued-key policy validator and the existing
SHA-256 schema. Plaintext newly issued keys are returned once and never stored.
Unknown root/entry fields, existing IDs, historical display metadata and other
provider secrets survive edits. Corrupt or missing databases fail closed rather
than being replaced with an empty database. Clearing a scope also removes its
legacy alias, preventing the backwards-compatible reader from restoring an old
restriction after an explicit clear. Readers still preserve existing aliases.

Mutations serialize with a bounded local admission gate and the existing inter-process apply lock,
then fsync and atomically rename a complete document while preserving ownership.
This deliberately requires consumers to reopen paths: **single-file Docker bind
mounts and non-participating legacy in-place writers are not compatible**. The
increment must not be enabled alongside such writers in production. The helper
does not yet provide durable request-ID deduplication. The opt-in configuration
transaction below adds cross-file recovery. Model probing/sync, service status,
private v8 management and quota/Reset operations remain to be migrated before
cutover. No Reset operation is used as a test.

The copied environment exposed a real capability-ordering fault: restoring a
different service UID before chmod required CAP_FOWNER. Staging permissions are
now set before restoring owner/group. The isolated integration keeps all
capabilities dropped except the same narrow multi-UID test capabilities; it
does not add CAP_FOWNER to work around the defect.

The final 2026-10-13 hyvps run passed 11 Go/Linux tests with no failures or skips
and 20 copied-environment integration checks. The real GUI login/key HTTP routes
and its unchanged IPC client exercised the Go helper. The Go gateway accepted a
GUI-issued key, rejected it immediately after disabling without a restart, and
retained all 13 models. Key edits and vault secrets survived helper restarts;
fixture deletion preserved the existing database records, unknown fields and
owner/group/mode. Audit-fault tests rejected mutations before dispatch. Windows
focused race tests, scoped Windows/Linux vet and the required server build passed.
The integration used local HTTP handlers, not a new external/TLS performance or
real inference test. Historical usage, quota history, OAuth files and account
configuration were unchanged; read-only production checks also confirmed stable
tracked files and service PIDs. All test containers used `--network none` and
were stopped; no real inference, refresh or Reset request was made.

## Go configuration compilation and recoverable application

The isolated helper supports `apply_bundle` when launched with an explicit
`--candidate-origin`. Compilation, static verification, publication and recovery
are Go code; no legacy Python builder or verification script is invoked. Both
the legacy YAML layout and v8's `api-keys.openai-compatibility` / `keys` groups
are supported. The original layout, comments and unrelated configuration nodes
are retained rather than serializing a typed config that would drop extensions.

The builder preserves non-GUI provider credentials, unknown provider/key/model
fields, unrelated context/generated records, global transport/access settings,
and all registered OAuth prefix boundaries. It refuses collisions with account
prefixes or preserved provider scopes, duplicate JSON/YAML fields, unsafe URLs,
missing vault keys and ambiguous multi-key managed providers. The existing
published connection tuple alone may survive DNS unavailability; a new URL or
proxy cannot authorize itself through draft state. Non-public DNS answers are
never accepted by that exception. This is apply-time origin validation, not a
claim of DNS pinning in every upstream executor. OpenCode Muse keeps its native
Responses routing. Pricing, issued keys, OAuth credentials and usage/quota history
are not configuration transaction targets.

Three independent renames are **not** simultaneous filesystem atomicity. The
transaction instead fsyncs private old/new backups and a journal, then arms a
shared configuration fence before publishing the three files. New inference
admissions return 503 while fenced. Authenticated `/v1/models` reads remain
available for control-plane validation. Readiness exposes the current file hash,
actual SDK credential/catalog registration, configuration readiness and admission
fence state separately; a listening port or a previous catalog cannot certify
the new bundle. Unchanged aliases also require matching upstream identity,
context window, explicit reasoning/image/output capabilities and display metadata
in the SDK registry, preventing a same-name stale catalog from passing. Confirmation requires the exact expected public catalog, keeping
unchanged OAuth and foreign routes and removing superseded managed routes.

Only after file durability and actual catalog verification does the journal
become committed and the fence clear. Failure restores only the three affected
configuration files and verifies the old SDK/public catalog before reopening
admissions. A failed catalog recovery retains the fence. On helper restart,
uncommitted or partially published journals restore verified old configuration;
committed journals verify the new configuration instead of restoring an old
snapshot. Corrupt backups or external edits cause a closed recovery failure,
never silent overwrite. A pending transaction prevents another apply. Queued
management requests and cross-process lock acquisition are cancelable, preventing
an expired queued key request from creating a key after an apply finishes.

This rollback is for a **failed user configuration edit**, not release rollback.
The eventual production deployment rollback must still switch only code/routes
and must not restore these configuration backups over newer business data.
Single-file bind mounts and legacy writers that do not participate in the lock
and fence protocol remain unsupported. The HTTP control adapter validates only
the private shadow contract; production hosting still needs its trusted private
control-plane identity and refresh-owner handoff. Neither executable gains a
production mode from this increment.

### Scoped verification

The copied ciyun environment passed 32 integration checks on hyvps. The original
GUI's actual apply HTTP route succeeded through the Go helper. Adding a fixture
alias changed the real SDK/public catalog from 13 to 14 models; removing it
restored the original 13 without losing either account. Static rejection left
published bytes unchanged. Pausing only the isolated Go candidate after the
durable fence tested a real local control-plane outage: old configuration bytes
were restored, rollback remained unconfirmed/fenced until the candidate returned,
and helper restart verified the catalog and completed recovery.

Linux helper tests passed 22 cases with zero failures/skips, including an actual
SIGKILL after publication before commit, partial three-file publication, corrupt
backup rejection, external-edit protection, committed-state recovery and canceled
queued mutations. Builder tests passed 16 cases/subcases; the final gateway
regression passed 52 cases/subcases, including canonical modality metadata and
stale same-alias capabilities as well as identity/catalog readiness. In total,
90 Linux tests/subcases passed with zero failures or skips. Windows focused race
checks, scoped vet and the required server build passed. The fault harness's
subprocess timeout was corrected to cover the complete apply/rollback window;
earlier harness failures were retained rather than counted as product passes.
All containers used isolated networks and private derivatives. No external
inference, OAuth refresh or Reset was performed; one local inference admission
probe was rejected by the fence. Production tracked files and service PIDs were
unchanged. The GUI/quota collector themselves remain Python, and remaining
management operations and production bootstrap/cutover are not complete.

## Authenticated private inventory and incremental Go GUI

The candidate now optionally exposes its management projection on a separate Unix
socket (`--candidate-control`, with an explicit peer UID). Both ends check Linux
`SO_PEERCRED`; a substituted socket owned by a different service UID is rejected.
The gateway socket is owner-private, in an owner-private real directory, and an
existing path is never blindly unlinked. Production supervision must provide a
fresh private runtime directory per gateway generation. The private handler is
not mounted on the public router and does not proxy arbitrary deprecated
management requests. It exposes readiness, the same authenticated SDK model
catalog, allowlisted provider/context inventory and account labels only.
Credential files, authentication indexes, bearer keys and arbitrary headers are
not inventory fields.

The helper's `status`, `gateway_models` and `gui_provider_seed` operations now use
that authenticated channel. Provider inventory supports both YAML layouts and
retains reasoning/image metadata. Status distinguishes the embedded Go SDK from
a separate Docker process; the embedded GUI understands this distinction rather
than incorrectly showing a healthy SDK as a stopped CLIProxyAPI container. The
older loopback adapter remains restricted to shadow validation and cannot enable
trusted inventory.

`cmd/cpa-gui` is an incremental, shadow-only Go GUI host. It embeds the existing
Chinese HTML/JavaScript interface rather than replacing its charts or preference
logic. The implemented HTTP routes cover login/logout/CSRF/password changes,
issued keys, current subscription inventory and draft CRUD, configuration apply,
model reads, prices, usage trends and current-cycle token-price billing. No Python
process is launched by this Go service. Remaining operations fail explicitly;
this command is **not yet a complete production GUI replacement**.

Authentication verifies the existing PBKDF2 or Argon2id password and persistent
signed session format. Process restart does not discard sessions. Logout revokes
the persisted entry; password change preserves pricing and unknown root fields
while incrementing the session epoch. New passwords require at least sixteen
characters. Argon2 parameters, session/request sizes, failed-login IP/global
buckets and GUI concurrency are bounded. Forwarded client addresses are accepted
only from explicitly configured loopback ingress peers. Mutations require the
exact HTTPS origin and a session-bound CSRF token. Session touches are batched at
one-minute intervals; across restart this can expire an idle session up to a
minute early, never extend its lifetime. Missing/corrupt auth or session documents
fail closed rather than silently generating a new password or database.

Draft edits keep existing unknown state fields, historical managed prefixes,
tombstones and durable pre-edit backups. The GUI does not store provider keys.
Model normalization is shared with the trusted compiler. Published configuration
is changed only by an explicit helper apply transaction, not by draft edits.
A draft-state persistence failure after a vault edit is reported as an uncertain
**draft** outcome, never a successful apply; cross-file draft/vault editing is not
claimed to be atomic. Automatic legacy backup/seed recovery is still pending.

Usage processing streams the existing JSONL with bounded line buffers and keeps
only bounded recent rows, aggregates and time buckets, not all historical raw
records. Explicit range queries retain zero-request wall-clock buckets and
preserve failed call metadata. Pricing continues to use the CPA Go port of the
original token-price calculation, not upstream billing normalization. The weekly
account projection uses the existing account attribution and cycle accumulator,
including manual reset markers. Additional snapshot fields are allowlisted again
at the GUI boundary. Official quota collection, reset management and empirical
quota fitting are still separate pending migrations; unavailable reset/fit data
is not represented as successful zero-valued data. The GUI does not claim to read
live official quota merely because a cached snapshot can be displayed.

Static HTML is precompressed with gzip and served with an ETag and short cache
lifetime. Dynamic authenticated responses remain `no-store`. No TLS/public network
performance improvement is inferred from these local checks.

### Copied-environment checks

The private inventory stage passed 38 isolated integration checks. The subsequent
Go GUI stage passed 45: the actual Go GUI login/key/apply HTTP routes, retained
three subscriptions, the original 13 models and both accounts, session refresh and
GUI process restart, real SDK model add/remove, and outage/journal recovery.
Usage totals, billing, model selection and wall-clock buckets matched the captured
Python implementation **when both read the same copied usage file**. The original
baseline path was absent in the isolated container; that harness input error was
fixed instead of treating an empty baseline as correct production statistics.

The first GUI layout attempt also exposed that making the gateway's private root
group-readable would violate its durable-auth ownership boundary. The final
fixture uses separate GUI and gateway hosting compartments, leaving the gateway
root private and the original OAuth mount read-only. This is a hosting test, not
proof of production systemd sandbox equivalence. Earlier failures remain private
host evidence and are not counted as passing checks.

Linux runs passed 22 helper, 20 builder, 52 catalog/readiness and 3 private-socket
tests/subcases. The final GUI-only regression passed 9 Linux tests, including draft
backups, rejection without state changes, secret projection, session revocation,
CSRF, time gaps and concurrency admission. Windows scoped race/vet and the required
server build passed. Test drivers can use Python to compare the old implementation;
the new GUI/helper/gateway service processes themselves are Go. All runs used
`--network none`, no real inference, OAuth refresh or Banked Reset was performed,
and production tracked files and service PIDs remained unchanged.

Still required before production cutover: provider probes and model synchronization,
automatic subscription recovery parity, official quota collection/reset wiring,
the empirical fit port, production host identities/private IPC wiring and actual
OAuth refresh-owner handoff. The isolation markers and `production_ready:false`
gates remain in place. Release rollback remains code/routes only and must not
restore older business snapshots.

## SDK-bound Go quota collection

The private `/quota` GET endpoint now uses each registered account's exact live
SDK credential. It checks the registry's member identity, disabled state, prefix
and workspace before reading the SDK access token. It never reads another
account as fallback, refreshes OAuth itself, or exposes auth indexes/token files.
The fixed official endpoint is `https://chatgpt.com/backend-api/wham/usage`; no
caller-selected URL/method/header is accepted. Redirects and environment proxy
inheritance are forbidden. Each account's proxy override or the configured global
proxy is retained, including cancelable socks5h acquisition. No connected-upstream
read/write timeout is introduced. The original caller's cancellation propagates.
A single quota batch is admitted at a time, with two account workers.

`cmd/cpa-quota-collector` is a Go-only, shadow-only replacement for the collector
process. It receives sanitized snapshots through the mutually authenticated Unix
channel, not OAuth files. It can run once or at a bounded polling interval. A
failed official query retains quota/update time only for the same account ID,
prefix, workspace and legacy attribution boundary. Its status remains unavailable;
unknown usage is nullable, not fabricated as zero. Current-cycle CPA price billing
continues to be owned by the unchanged billing accumulator, independent of quota.

The Linux store anchors all file operations to a directory descriptor and uses
no-follow, descriptor-relative opens/renames, rather than trusting a mutable
privileged path after inspection. It preserves existing public file permissions
and ownership, fsyncs the staged snapshot, atomically replaces it and fsyncs the
directory. Corrupt snapshots fail closed. A process-lifetime kernel lock excludes
another participating Go collector. Legacy collector timers must be stopped by
supervision before this becomes the production writer; the lock does not magically
coordinate writers that ignore it.

History maintains the existing sanitized JSONL schema and half-open account/time
exclusions. Only successful official weekly-window observations enter the fit
history. Invalid exclusions or an incomplete historical line are reported without
erasing history or invalidating an otherwise successful snapshot. Rotation never
overwrites `.1` or any prior archive: it uses a fresh random archive name with
no-replace rename and preserves exact old bytes. Historical pricing, requests,
OAuth files and providers are not store targets. The command is not yet a
production bootstrap or refresh-owner handoff mechanism.

### Verification and limits

Fifteen Linux quota tests and 53 Linux gateway/catalog tests passed with no
failures/skips. Quota tests include fixed endpoint/headers, bounded responses,
no credential/error-body leakage, socks5h and environment proxy behavior, two
members sharing a workspace without rotation/fallback, stale quota matching,
permissions, corrupted snapshots, exclusions, incomplete lines, non-overwriting
rotation, kernel ownership and directory/file symlink substitution.

The copied-environment run passed 53 integration checks. It ran the actual Go
GUI/helper/gateway and standalone Go collector processes. The container had
`--network none`: official acquisition correctly returned unavailable, retained
both identities and their prior quota/update epochs, left fitting history
unchanged, and changed no OAuth token, provider secret, issued key or request log.
The GUI consumed the collector snapshot with cycle billing intact. Public `/quota`
returned 404; the private endpoint stayed off the public router. Existing model
apply/add/remove/recovery and session checks also remained green. Production
tracked files and service PIDs stayed unchanged.

Successful official-response behavior is currently verified with synthetic
RoundTripper responses, **not** a live official quota HTTP test. No real inference,
OAuth refresh or Banked Reset operation was performed. Scoped Windows race/vet,
Linux compilation/unit execution and the required server build passed. Empirical
quota fitting, Reset management, model sync/probes, automatic subscription recovery
and the actual production identity/refresh-owner/cutover gates remain pending;
`production_ready:false` and shadow guards remain intact.

## Go empirical quota fitting engine

`internal/chiphalo/quotafit` now ports the existing `quota_model.py` numerical
engine. It is not a replacement by a simpler linear model and does not import
pricing or billing packages. It has no file, credential, network or Reset access.
The host supplies the fitting epoch and input observations.

Implemented contracts include conservative model aliases, raw/integer percentage
normalization, account-local cycle/reset segmentation, coverage and lag windows,
the original (start,end] token boundary, observation caps and skip diagnostics.
Model insertion order is preserved internally for tied model selection. The
weighted Gram/Cholesky Lawson-Hanson NNLS solver keeps non-negativity, singular
system jitter and active/passive ordering. Huber reweighting, ridge, rank/condition
and collinearity diagnostics retain their existing thresholds.

The complete engine also includes per-model and shared-ratio pooled structures,
coordinate refinement, relative Luna reference weights, bootstrap intervals and
stability, outlier exclusion/refit, time-ordered held-out validation, per-cycle
billing-regime drift and optional request-overhead diagnostics. Request overhead
is diagnosed, not silently enabled. Reports retain the existing status/flag,
coefficient, interval, metric, point, note and summary fields; insufficient data
is not reported as a successful zero coefficient estimate.

Bootstrap uses a port of CPython's integer-seeded MT19937/getrandbits rejection
sampler, not Go's unrelated default PRNG. This makes seeded resampling and report
comparisons reproducible across the language-only rewrite, including negative and
large integer seeds. The engine does not feed empirical quota coefficients into
the independently preserved token-price cost calculation.

### Numerical evidence and remaining integration

Seventy-one synthetic differential fixtures were generated from the captured
Python implementation: 25 observation/numerical cases, 31 structure/bootstrap/PRNG
cases and 15 full-report cases. They cover empty/invalid snapshots, raw fractional
and rounded percentage feeds, clock gaps, reset cuts, lag shifts, duplicate sample
times, independent/singular/zero/outlier designs, pooled and mixed structures,
tied model order, request overhead, stable/drifting/shifted cycles, manual/outlier
exclusions, model drops and disabled diagnostics. They contain no production
requests, credentials or account identities. Full JSON structure, flags and values
are compared recursively; finite numerical fields use the documented test
absolute/relative tolerance of 2e-7, rather than claiming bit-for-bit arithmetic.

The Linux run passed 75 tests/subcases with no failures/skips, using the captured
portable image, `--network none`, an unprivileged UID, dropped capabilities and
synthetic fixtures only. Scoped Windows race/vet and the required server build
passed; the unchanged billing package's race tests stayed green. Generating the
reference fixtures uses Python outside the fork; the engine and its tests are Go.

This engine is not yet exposed by `/api/quota-model`. The GUI wrapper still needs
its account/time/exclusion selection, report union/compaction, async single-flight
cache and copied-production-history differential validation. No live official
quota, OAuth refresh or Reset was performed, no production host was modified,
and the production hosting/cutover gates remain closed.
