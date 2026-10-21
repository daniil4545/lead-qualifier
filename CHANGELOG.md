# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-10-21

Checks of what the model says, and reliability when the model or its gateway fails.

### Added

- Claim grounding (`internal/agent/claims.go`): a reply that says a meeting is booked, moved or
  cancelled, that a manager is on the line or that the bot will return on a date is checked
  against the live booking and the turn outcome. An ungrounded draft is regenerated once with a
  remark, then the sentence is cut. A draft that was only claims becomes the booking text from the
  database (`meeting_alive`), the windows of the day the lead named, or a handoff
  (`claim_dropped`). The cut classes are written to the decision audit.
- A bare confirmation ("ок", "спасибо", "до встречи") from a lead with a booking ahead is answered by
  a catalog text (`meeting_ack`) without a model call; a repeat after that text stays silent.
- `prompteval -max-usd` (default 2): a spending ceiling over all model calls, the critic and the
  niche classifier included. A run that reaches it writes a partial report with the cost and exits
  with code 3. The report carries `cost_usd` and `max_usd`.
- The raw answer of a critic that broke the verdict format is kept in the decision audit
  (`critic_raw`).
- `make vuln` and a CI step with govulncheck; gosec runs inside golangci-lint.

### Changed

- A failure on the model gateway side (outage, rate limit, empty balance) no longer hands the
  case to a human after three attempts: the turn is deferred, with a backoff of up to 16 minutes,
  until the lead's oldest unanswered message is 24 hours old by the database clock. The ceiling of
  three attempts stays for technical outcomes of what the model wrote.
- A completion cut by the token cap (`finish_reason: length`) is treated as an empty answer: it
  never reaches the critic or the history, the call repeats once, a second stump is
  `empty_content`. The default cap is 2400 tokens instead of 1200.
- A line of a single hyphen next to real text is removed before the checks, so it cannot become
  an empty bullet in front of the window list.
- A forbidden number in a golden fixture is not taken from the middle of a longer number.
- `texts.json` requires two more texts: `meeting_alive` and `meeting_ack`.
- `golang.org/x/sync` 0.22.0; report files of `prompteval` are written with mode 0600.

### Security

- `golang.org/x/text` 0.41.0 fixes GO-2026-6629 and GO-2026-5970, reachable through the Postgres
  pool.

## [0.1.0] - 2026-10-21

First public release: a reference implementation, not a deployable product.

### Added

- Lead qualification agent: a model turn loop with native tool calling over OpenRouter
  (`search_kb`, `save_facts`, `complete`, `offer_slots`, `book_slot`, `escalate`, `refuse`,
  `stop_contact`), a deterministic meeting gate, booking with a unique index against double
  booking, and an outbox for the reply, the CRM note and the CRM task.
- Guard (deterministic filters) and critic (separate strict-JSON model call) over every text
  draft, each with its own regeneration budget.
- Catalog directory (`CATALOG_DIR`): company, persona, tariffs, niche segments, managers, texts,
  prompts and knowledge base as files, validated at start. A second catalog in
  `testdata/catalog-alt` runs through the same tests.
- Console demo (`make demo`) with a fake in-memory CRM.
- Offline demo (`make demo-offline`, `lead-qualifier demo --offline`): the same pipeline on a
  scripted stand-in for the model, no API key and no network. Covered by an end-to-end test.
- `prompteval`: the golden fixtures (15 classes) through a live model n times, with pass rate,
  unsafe outcomes and divergence classes.
- CI: build, vet, gofmt, golangci-lint and the full test suite against a Postgres service.
