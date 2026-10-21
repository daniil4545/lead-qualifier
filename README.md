# lead-qualifier

A lead qualification engine for B2B services. A bot talks to a person who left a request: it answers questions from a knowledge base, collects five facts about the company, decides whether to invite the lead to a meeting, and books a slot with a manager. The LLM runs the conversation and brings facts; deterministic Go code makes every decision.

The project is small enough to read in an evening: a console demo, a fake CRM, Postgres in Docker, an OpenRouter key. It was extracted from a production service. The customer is replaced by a fictional company ("Тихая гавань", a managed IT support vendor), and all business data in `data/example` is made up.

User-facing texts are in Russian: the agent talks to Russian-speaking leads, so the prompts, the catalog content (knowledge base, ready replies, tariff and niche names), the golden dialogs and the demo prompts stay in Russian. Everything meant for developers is in English.

## Quick start

You need Go, Docker and an OpenRouter key.

```sh
export OPENROUTER_API_KEY=...
make demo
```

The demo asks for the lead's name, creates a request in the fake CRM, and the bot writes first. After that every input line is a message from the lead. CRM records are printed as `[CRM <kind> <lead>] <text>` lines, logs go to `demo.log`. The default model is `google/gemini-2.5-flash`, set in `data/example/config.json` and overridable with `OPENROUTER_MODEL`.

## How a turn works

One agent turn is a loop of model calls with native tool calling. The tools are `search_kb`, `save_facts`, `complete`, `offer_slots`, `book_slot`, `escalate`, `refuse` and `stop_contact`. The loop writes nothing itself: it returns an untrusted result, and the `dialog` package validates it and applies it.

1. `store.Ingest` saves the lead's message and queues the case.
2. `agent.RunTurn` runs the model with tools; the guard and the critic check every text draft.
3. Go re-checks the turn and computes the gate; `store.ApplyDecision` writes the decision, status, booking and outbox rows in one transaction.
4. `dialog.DeliverNext` sends the reply to the lead and writes the note and task to the CRM.

Details of the case model, the gate outcomes and the invariants are in [docs/architecture.md](docs/architecture.md).

## Engineering decisions

The central idea: the model brings facts and writes text, the code decides. Every item below is something that can fail in a lead-facing bot, what I did about it, and what I rejected.

1. **Decisions are code, not prompt.** The meeting gate computes the niche segment, the revenue thresholds and the tariff from the catalog: the price from the lower bound of the revenue range, the admission from the upper bound. The same facts always give the same outcome, so the gate is covered by ordinary table tests. Rejected: putting acceptance rules into the prompt. A prompt can set the tone, but a rule that lives there can be talked out of the model.
2. **Tool refusal is a tool answer, and it is checked twice.** Slots and booking are unavailable before the niche verdict, before the full set of facts, with a closed gate and with an already booked meeting. The agent answers a forbidden call with the reason, so the model understands and continues the conversation. `dialog` then re-checks the same boundary before booking, because the turn result is untrusted: a stub or a model failure must not produce a meeting that bypasses the gate. Rejected: rebuilding the tool schema per state, which gives the model no explanation, and trusting the model not to call a tool it was told is unavailable (a prompt injection asks for exactly that, and there is a test for it).
3. **Two independent filters on every draft.** The guard is a set of deterministic filters: foreign prices, amounts when the tariff is unknown, names of third parties (with Russian cases and in transliteration), phones, e-mail, links outside the catalog, retelling of internal rules. The critic is a separate strict-JSON model call that checks facts against the issued knowledge base fragments, promises and manipulation. A guard false positive costs one regeneration, a miss is a leak, so the filters are stricter than necessary. A rejected draft goes back to regeneration with a specific remark per category, never to the lead. Each filter has its own regeneration budget (two), because a shared budget let the critic use up the guard's attempt. Rejected: the critic alone, which is not reproducible and cannot carry a test per rule.
4. **Untrusted input everywhere.** Facts are sanitized and length-limited before they enter the trusted context. Time and price are printed by Go and the model only repeats them. Remarks from the code are marked with a prefix so the model does not answer them aloud, and the guard has a separate echo filter for the case when it does. The niche is classified by a separate model call, not by a `save_facts` field: the dialog model edits facts every turn and would rewrite the code to fit the conversation. An unclear classifier answer gives an unconfirmed profile and a meeting, never a refusal.
5. **A model failure is not a decision.** A transport error, an empty answer, two broken `save_facts` in a row or a critic error defer the turn (1, 2, 4 minutes) and replay it. After three deferrals the case goes to a human with a ready reply and a CRM task. Rejected: letting a failed turn fall through to "no answer" or to a guessed outcome.
6. **State is a projection plus a journal, delivery is an outbox.** The case row is a projection; `decisions` and `case_events` are the journal. The reply to the lead, the CRM note and task, and the booking are written in the same transaction as the decision, and delivered afterwards from the outbox with the row key as the idempotency key. A double booking is stopped by a unique index on (manager, start), not by window selection: when two turns race for one window, the loser gets fresh windows instead of a confirmation of a booking that does not exist. Rejected: calling the CRM or the transport inside the turn.
7. **Business data lives in a catalog directory.** Company, persona, time zone, work hours, tariffs, niche segments, managers, ready texts, prompts and the knowledge base are files in `CATALOG_DIR`. A test fails if a catalog value appears as a literal in Go code, and a second catalog (`testdata/catalog-alt`: other company, thresholds, tariffs, zone, meeting length and persona) runs through the same table tests as the first. The five-fact schema stays in code: a data-driven schema would add nothing and make the gate harder to test.
8. **The knowledge base is validated at start.** Anything in it the model may repeat verbatim, so startup rejects prices that are not catalog tariff prices, links outside the catalog, phones, e-mail and stop names, and requires the table of contents to list exactly the existing sections. The catalog itself is checked too: a broken catalog refuses to start, because a running service with it would name a wrong price or time to a lead.
9. **Offline evaluation first, live model second.** `data/example/golden` holds 15 classes of one-turn fixtures (full facts, knowledge base question, injection, price insistence, slot choice, reschedule, and so on). Offline, the fixtures are checked for consistency against the tool contract and the catalog. `prompteval` runs them through a live model n times and reports the pass rate, unsafe outcomes and divergence classes; transport failures are counted separately and fail the run. A fixture checks the action, the tool arguments, the facts snapshot and forbidden substrings. It does not judge the quality of the text: that stays with the critic and a human.
10. **Boring infrastructure.** JSON config on the standard library, one idempotent migration at start, `pgx` for Postgres, no framework, packages split by domain boundary. Rejected: deferred follow-up touches for silent leads (they need a scheduler, and the project has none) and a refusal that waits for revenue growth, for the same reason.

## Catalog: your own business without code changes

Everything that differs between customers is in the catalog folder (`CATALOG_DIR`, default `data/example`):

- `config.json` - company, persona and its gender, time zone, work hours, meeting length, model, tariffs (threshold, name, price), niche segments with thresholds, refusal reasons, managers, links;
- `texts.json` - the greeting and ready replies of the code;
- `prompts/` - agent, critic, niche classifier;
- `knowledge/` - the knowledge base for `search_kb`; `stop_names.txt` - whom the bot never names;
- `golden/` and `niche_cases.tsv` - evaluation fixtures.

To plug in your own business, copy `data/example`, fill it in and run with `CATALOG_DIR=path`.

## Prompt evaluation

```sh
make prompteval ARGS="-n 3"
```

Every fixture from `data/example/golden` is run n times through a live model. The report shows the pass rate, unsafe outcomes and divergence classes. Runs that failed on transport are counted separately and make the command exit with an error. `-prompts` evaluates a candidate set of prompts against the same knowledge base, `-out` saves the report as JSON.

A fixture looks like this (shortened; texts are Russian):

```json
{
  "class": "happy_path",
  "case_status": "collecting",
  "messages": [{"author": "lead", "text": "У нас сеть из четырёх магазинов стройматериалов в Бердске, выручка около 12 млн в месяц. Я генеральный директор и владелец компании."}],
  "expected_action": "complete",
  "expected_facts": {"monthly_revenue": "12000000", "is_owner": true, "city": "бердск"}
}
```

## Tests

```sh
make test-fast   # no database
make test        # with Postgres from docker-compose
make check       # gofmt, vet, golangci-lint, full tests, build
```

The live niche classifier test over `niche_cases.tsv` is enabled with `LQ_LIVE_MODEL=1` and needs `OPENROUTER_API_KEY`. CI runs build, vet, gofmt, golangci-lint and the full test suite against a Postgres service.

## Packages

| Package | Contents |
|---|---|
| `config` | environment and catalog, validation at start |
| `qualify` | case and statuses, facts, tariffs, niche, gate |
| `knowledge` | knowledge base and its validation |
| `crm` | CRM interface and an in-memory fake |
| `calendar` | free windows of managers |
| `agent` | turn loop, OpenRouter, guard, critic, niche classifier, prompts |
| `store` | Postgres: cases, messages, decisions, bookings, outbox |
| `dialog` | handler: case claim, turn, applying the decision, delivery |
| `console` | demo transport |
| `eval` | golden fixtures and turn evaluation |

Commands: `cmd/lead-qualifier` (`demo`) and `cmd/prompteval`.

## Status and limitations

- A working reference implementation, not a deployable product. The only transport is the console, the only CRM is an in-memory fake; real integrations would implement the `Sender` and `crm.CRM` interfaces.
- One process, one database. There is no scheduler, so a silent lead gets no follow-up touches and a refusal on revenue closes the case.
- The golden fixtures cover one turn per class, not whole conversations. I have not published pass rates for any model: run `make prompteval` against the model you intend to use.
- The guard filters and the catalog texts target Russian; another language needs new patterns, prompts and texts.
- The knowledge base and all business data in `data/example` are fictional.

## License

MIT, see [LICENSE](LICENSE).
