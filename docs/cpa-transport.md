# CPA compressed and incremental transport

This is the Go **candidate**, based on CLIProxyAPI v8.0.25. It does not replace
the running production Python gateway/GUI. `cmd/cpa-gateway` still refuses
general production startup: the remaining migration/security gates are not
removed by these transport features. Do not deploy it over the production service.

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
