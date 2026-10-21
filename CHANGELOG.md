# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
