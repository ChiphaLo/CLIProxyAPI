# Claude request conversion performance (fork-specific)

This fork carries optimizations of the OpenAI Responses → Claude Messages
request path that upstream does not have. They were introduced in `6275e0c7`
on top of the upstream sync merge `f05efbde` (upstream `f5c5681a`). Upstream
keeps editing the same files, so an upstream merge can silently drop them by
taking the upstream side of a hunk. Every upstream sync must preserve the
invariants below and repeat the checks in the last section.

## Measured effect

Loopback benchmark on the 9950x deployment image (2 vCPU, 768 MB), mock Claude
upstream, conversion time = client send start → upstream body received, p50 of
30 runs, cloak mode `always`:

| fixture | before (`f05efbde`) | after (`6275e0c7`) |
|---|---|---|
| text-small (0.04 MB, 32 tools) | 3.2 ms | 2.9 ms |
| documents (6.5 MB: 3 PDFs + 5 text files) | 71.2 ms | 39.8 ms |
| images (6.9 MB: 24 screenshots incl. one 4K) | 99.6 ms | 41.3 ms |
| mixed-agent (5.8 MB: 243 input items) | 108.1 ms | 70.2 ms |

A sync that loses any invariant below shows up as a regression of the
corresponding fixture, mostly documents/images (media) or mixed-agent
(message history).

## Invariants

### 1. CCH signing streams its input

`internal/runtime/executor/claude_signing.go`

- `writeClaudeCCHInput(w io.Writer, body)` writes the normalized signing input
  straight into the xxHash writer. `normalizeClaudeCCHInput` is only a wrapper
  around it. Do not reintroduce a normalized copy of the whole body.
- `claudeCCHJSONScanner.parseString` jumps with `bytes.IndexByte` to the next
  quote or backslash; it must not walk strings byte by byte.
- Object member lists use the scanner's reusable stack (`members`,
  `membersStart`), not a slice allocated per object.

### 2. Stream scanners skip plain string runs

`jsonStringSpecialIndex` (`claude_executor_cloaking.go`) is used by
`claudeStreamDocScanner.Write` and `xaiImageRefScanner.Write`
(`xai_executor_request.go`) to skip ordinary string bytes in bulk.

### 3. Edited documents are never read as a whole `messages` array

After any edit, `doc.Get("messages")` on an `*ojson.Doc` re-serializes the
entire array (MB of base64 per call). Message history must be read through:

- `util.ForEachDocArrayElement(doc, path, visit)` (`internal/util/ojson_doc.go`)
- `claudeDocMessageList(doc)` (`claude_executor_cloaking.go`) when random
  access to messages is needed

Current users, all of which must keep using them:

- `isClaudeTitleHelperRequestDoc`, `isClaudeNewPromptTurnDoc`,
  `claudeHistoryHasAdvisorCallOrResultDoc` (per-message
  `claudeMessageHasAdvisorCallOrResult`),
  `insertClaudeMidConversationSystemBlocksDoc`,
  `claudeMidConversationSystemMessagesAtEndDoc`
- `claudeDocHasSignedThinkingBlock` (per-message
  `claudeMessageHasSignedThinkingBlock`), `remapOAuthToolNamesWithOptionsDoc`,
  `sanitizeClaudeMessagesForClaudeUpstreamDoc`
- `helps.NormalizeClaudeToolCallIDsDoc`,
  `helps.NormalizeClaudeToolPropertyNamesDoc`
- `signature.RestoreCodexCommentaryMarkersDoc`

When upstream adds a new `doc.Get("messages")` (or `.Array()` over it) inside
the Claude executor path, convert it the same way.

### 4. Raw arrays are built once and handed to the document

`joinRawJSONArray` builds a JSON array in one allocation and
`util.SetDocRawOwned` passes the buffer to the doc without copying. Do not
restore `doc.SetRawBytes(path, []byte("["+strings.Join(parts, ",")+"]"))`
in `claude_executor_cloaking.go` (including
`injectClaudeCodeCurrentDateInternalDoc`).

### 5. Large media stays deferred until the final body

`internal/translator/common/deferred_media.go`

- `ClaudeExecutor.Execute` and `ExecuteStream` create a
  `translatorcommon.DeferredMedia`, pass it to translation via
  `translatorcommon.WithDeferredMedia(ctx, media)`, and hydrate once with
  `bodyForUpstream := media.HydrateJSON(doc.Bytes())` after payload rules and
  before signing.
- The Responses translator (`responses/deferred_media.go`,
  `newDeferredMedia(common.DeferredMediaFromContext(ctx))`) leaves handles in
  its output when the caller supplied the store, and hydrates itself only when
  it did not.
- Image size checks and resizing read the real data through
  `helps.ResizeClaudeImagesDocResolving(doc, media.Resolve)`.
- `pluginhost.(*Host).NormalizeRequest` hydrates before handing the body to a
  request-normalizer plugin, and clones only when such a plugin exists.
- `appendDocJSONString` must keep encoding exactly like `ojson.Doc.Set`
  (HTML escaping, invalid UTF-8 → `\ufffd`); the hydrated body must be
  byte-identical to translator-side hydration.

Any new code between translation and `HydrateJSON` that reads `source.data`
of image/document blocks must resolve it through `media.Resolve`.

### 6. Avoid needless whole-body copies

- `stripTranslatedDefaultMaxTokens(cfg, ...)` returns early when
  `helps.PayloadRulesConfigured(cfg)` is false.
- `util.ResponsesToolReverseIdentityMap` parses without copying and
  `strings.Clone`s every string it returns.

### 7. One pass per large Responses input item

Responses input items have arbitrary key order, so each missing or trailing
key lookup skips the whole item content. Read several keys with
`ojson.GetMany(item.Raw, ...)`:

- `responsesItemTurnBoundary` and `scanResponsesWebSearchHistory`
  (boundary/type computed once per item)
- `isCodexDelegationToolOutput` (`translator/common/responses.go`)
- the main item loop of `convertOpenAIResponsesRequestToClaude`
  (`role` and `type` read together)

## Upstream sync checklist

1. Resolve conflicts in the files listed above by keeping the fork-side
   structure, then re-apply upstream's semantic changes on top of it.
2. Search the Claude executor path for regressions of invariant 3:
   `rg -n 'Get\("messages"\)' internal/runtime/executor internal/signature`
   and review each new hit.
3. Run the guarding tests:
   `go test ./internal/util/ ./internal/translator/... ./internal/signature/ ./internal/pluginhost/ ./internal/runtime/executor/...`
   (`ojson_doc_test.go`, `TestDeferredMediaCallerHydrationMatchesTranslatorHydration`).
4. Re-run the loopback benchmark and compare the upstream request bodies
   against the previous build (normalize only random IDs, `cch` and the
   `cc_version` suffix): every fixture must be byte-identical and timings must
   stay within noise of the table above.
