# QR login and message continuation implementation plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task by task.

**Goal:** Concentrate QR login observations and message-continuation rules behind smaller interfaces.

**Architecture:** LoginFlow exposes Observe, Result, SubmitPassword and Abort. QRFlow owns coherent observations and final-result readiness; authsrv retains OAuth finalisation and its existing adapter. Three concrete page preparers own validation, cursor restoration and continuation state; handlers retain provider calls and presentation.

**Tech Stack:** Go, gotd/td, MCP Go SDK, existing scripted Telegram test adapter.

**Spec:** The design approved in this conversation on 2026-10-20 (user: “поехали”); the Architecture and Global Constraints sections capture that design.

## Global Constraints

- Preserve HTTP and MCP inputs/outputs, password-attempt semantics, cursor wire shape and version.
- Preserve gotd/OAuth isolation, allowlist before persistence, single consumption, cancellation-independent bounded persistence and failed-commit cleanup.
- No compatibility aliases, new dependencies or generic pagination framework.
- Scope cursor work to GetMessages, SearchMessages and GetReplies.
- Record the selected domain terms in GLOSSARY.md without implementation details.
- Work in the existing Conductor worktree; preserve .serena and the branch name. Leave changes uncommitted for review.

## Review Focus

- Concurrent state reads must not combine phases, revisions and counters from different observations.
- Session data must be final before success is observable and must never enter ordinary observations.
- Concurrent successful polls must persist and mint a code once.
- Cursor-only continuation must retain every original filter, including booleans and thread roots.
- Invalid inputs, scheduled partial outcomes and raw continuation anchors must retain their existing behaviour.

### Task 1: QR login observations

**Files:** internal/tgclient/qrflow.go and tests; internal/authsrv/login.go, bootstrap.go and tests; internal/server/httpauth.go and tests.

**Interfaces:** Observe() returns a snapshot of state, token/revision, rejection counters and terminal error. Result() returns the completed identity/session and readiness without blocking. SubmitPassword and Abort retain their contracts.

- [x] Add observation/result tests: repeated/rotated tokens, readiness, defensive session copy, coherent counters. Run targeted tests and observe the missing-interface failure.
- [x] Implement coherent observation and result publication. Remove superseded getters and external Done waiting; adapt HTTP callers, adapter and scripted fake.
- [x] Exercise real flow password transitions and HTTP single-finalisation/allowlist/cancellation/cleanup scenarios. Run go test ./internal/tgclient ./internal/authsrv ./internal/server.

### Task 2: Message continuation

**Files:** internal/tools/message_page.go (new), messages_get.go, messages_search.go, replies_get.go, message_page_cursor.go and relevant tests.

**Interfaces:** prepareHistoryPage(GetMessagesInput), prepareSearchPage(SearchMessagesInput), prepareRepliesPage(GetRepliesInput) return mode-specific prepared pages and validation results. Each page owns its cursor state and continuation method; matching provider calls stay in handlers.

- [x] Add tests proving filter round trips, mixed-input rejection and literal v1 cursor stability; run them before implementation.
- [x] Move preparation from handlers into the three concrete preparers, sharing existing cursor/date/handle validation. Preserve validation text/order and query trimming.
- [x] Keep existing full MCP pagination tests and scheduled-result tests. Run go test ./internal/tools ./internal/messages.

### Task 3: Whole-change verification and review

- [x] Run go test ./..., targeted race tests, go vet ./..., and golangci-lint for macOS and GOOS=linux.
- [x] Obtain a fresh independent review; fix confirmed issues and rerun affected checks.
- [x] Check the final diff and worktree; report changes and actual verification results.
