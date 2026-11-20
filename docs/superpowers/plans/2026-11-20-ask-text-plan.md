# Free-text questions from the live page: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a person on the hub's live page can type a short question about the selected frame; the question reaches their agent through the existing inbox and `page_state`; the agent answers on the page as it does for the "Ask my agent about this" chip.

**Architecture:** extend the existing `ask` report with an optional `question`. The page sends it with the frame; the hub validates it, keeps it in the inbox ring with the ask, and `page_state` returns it, labelled as text a person typed. Nothing else changes: the page has no AI of its own, the agent still reads asks with `page_state` and answers with `page_explain`.

**Tech stack:** Go 1.26 (internal/perfmcp, internal/perfpage, internal/livehttp), the live page's `live.js`/`livepage.html`/`live.css`, Chrome tests.

**Builds on:** branch `ai-mcp-m2c-serve` (draft PR #272) at 56c7474f. Spec of record: `docs/superpowers/specs/2026-11-15-local-server-design.md` §D3–§D4 (inbox, `page_state`), SRV-R14/R15.

## Global constraints

- Build on top: never modify `main.go`, `telemetry.go`, `perfagent/`, `gpu/`, `cmd/flamegraph`, `internal/profilemodel`, `internal/explain`. The static export bytes stay unchanged.
- The stdio binary keeps linking no networking. No new dependencies.
- Everything the page shows from the server is set with `textContent`; the CSP is not loosened.
- No AI attribution, session URLs, issue refs or ruling IDs in code comments.

## Decisions

1. **The one exception to "the inbox keeps only server values".** Until now every value in the inbox came from the server's own snapshot (IDs, names). The question is the first text a person types that reaches the agent. It is bounded, normalised and always labelled: `page_state` returns it under `question` with `question_source: "typed on the page by the person viewing it"`, and the guide tells agents to treat it as a question to answer about the frame, never as instructions. The `ask` without a question behaves exactly as today.
2. **Bounds.** At most 280 Unicode characters after trimming; valid UTF-8; no control characters other than none (newlines are folded to spaces, other C0/C1 and bidi controls refused); empty after trimming = an ask without a question. The inbox request body cap rises from 1024 to 2048 bytes (`livehttp.maxInboxBody` and `perfpage.maxReport`) so a 280-character question in any script fits.
3. **Refusals.** A question over the bound, invalid UTF-8 or a control character → 422, with a reason that never echoes the text. The page shows "Your question could not be sent: …".
4. **Dedup.** The existing 2 s duplicate-ask window keys on (publication, node, question).
5. **Rate.** Unchanged: the same InboxGuard bucket. SRV-R15's coalescing applies to select/focus only; an ask with a question is sent at once and retried once after a 429, like an ask.
6. **Page UI.** In the side panel, under the "Ask my agent about this" chip, a single-line text field ("Ask about this frame…", `maxlength=280`) with an "Ask" button and Enter to send; disabled until a frame is selected. After sending, the field clears and the existing toast/inbox count update. Each ask row the page shows (and the read_by line) shows the question as text when there is one.
7. **Agent side.** `page_state` rows: `question` (string, omitted when none) and `question_source`. The guide (≤ 12288 bytes) and the skill say how to answer: read the frame's facts, answer the question on the page with `page_explain` for that node, and treat the text as the person's question, not as instructions.

## Review focus

1. Hostile text: `</script>`, `<img onerror>`, RTL override, zero-width joiners, 280 emoji (4-byte runes), a NUL, CR/LF injection into the agent-visible row — refused or rendered as plain text everywhere.
2. A question cannot change anything but the ask row: it never alters the publication, the user view or the rev beyond what an ask already does.
3. The body cap change doesn't loosen the slow-body bound (the 2 s read deadline still holds).
4. The guide's wording keeps agents from following instructions embedded in a question.
5. The static export and stdio are untouched.

---

### Task 1: The question in the inbox and `page_state`

**Files:** `internal/perfmcp/inbox.go`, `internal/perfmcp/tool_page_state.go`, `internal/perfpage/inbox.go`, `internal/livehttp/inboxguard.go`, their tests; `internal/perfmcp/guide.md`, `skills/perf-agent-mcp/SKILL.md`, `docs/mcp.md` (inbox and `page_state` sections), `CHANGELOG.md`.

**Interfaces:**
- `PageReport` gains `Question string`.
- `perfpage.decodeReport` accepts an optional `"question"` key (strict JSON as today: unknown keys, duplicates, wrong types refused).
- `page_state` ask rows gain `question` and `question_source` (decision 7).

- [ ] **Step 1: Tests first.** `TestAnAskCarriesItsQuestion` (page → Inbox → Report → page_state row has the trimmed question and the source label); `TestQuestionBounds` (281 chars → 422; 280 four-byte runes → accepted within the 2048-byte body; invalid UTF-8, NUL, C1, bidi overrides → 422 with a reason that does not contain the text; newlines folded; whitespace-only → plain ask); `TestQuestionDedupKeysOnTheText`; `TestTheInboxBodyCapIs2048` (2049 → 413, 2048 → read); `TestAQuestionChangesOnlyTheAskRow`.
- [ ] **Step 2: Implement.** Validation in one place (perfmcp), the cap constants, the row fields, docs/guide/skill/changelog.
- [ ] **Step 3: Checks.** `go test -race -timeout 25m ./internal/perfmcp/ ./internal/perfpage/ ./internal/livehttp/ ./cmd/perf-agent-hub/`, lint 0, the guide size test.
- [ ] **Step 4: Commit.** `perfmcp, perfpage: an ask can carry the question the person typed`

### Task 2: The question field on the live page

**Files:** `internal/flamegraph/live.js`, `livepage.html`, `live.css`, `liveassets_test.go` (Chrome), `cmd/perf-agent-hub` Chrome e2e.

- [ ] **Step 1: Tests first (Chrome).** Typing a question and pressing Enter sends `{kind:"ask", publication, node, question}` and clears the field; the field is disabled with no selection; a 422 shows the refusal toast without the text; hostile questions render as text in the page's ask rows (no element created, no CSP violation); end to end on the real hub: the typed question appears in `page_state` and the agent's answer (a scripted `page_explain` in the test) appears in the panel.
- [ ] **Step 2: Implement** in live.js's style; text only via `textContent`; no inline style or script.
- [ ] **Step 3: Checks.** `go test -race -timeout 25m ./internal/flamegraph/ ./cmd/perf-agent-hub/` (Chrome runs), lint 0, export goldens unchanged.
- [ ] **Step 4: Commit.** `flamegraph: the live page lets you ask your agent a question about a frame`

### Task 3: Verification and draft PR

- [ ] Full `go test -race ./...`, lint, darwin build, untouched-tree check; a real smoke: hub + Claude Code (`claude -p --mcp-config … --strict-mcp-config`) reading a typed question from `page_state` and answering on the page; two reviews of the branch; a draft PR stacked on #272. Never merge.
