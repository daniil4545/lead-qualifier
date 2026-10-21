# Architecture

One process, one database. A lead writes to a transport, the handler claims the case, the agent makes a turn, the code validates it and writes the decision in a single transaction together with the outward actions. Delivery runs in a separate loop from the outbox.

The path of one message:

1. `store.Ingest` saves the message and queues a live case for a turn.
2. `dialog.ProcessCase` claims the case. A bare confirmation of a live booking ("ок", "спасибо") is answered by a catalog text right here, without the model. Otherwise `agent.RunTurn` makes the turn: model, claim check, guard, critic.
3. Go validates the turn and computes the gate, `store.ApplyDecision` writes the decision, status, booking and outbox.
4. `dialog.DeliverNext` delivers the reply to the lead and the note and task to the CRM.

## Case model

There is one case per lead. The status is a projection, `decisions` and `case_events` are a journal for analysis.

| Status | Incoming message | Agent turn | Model failure | Outbox delivery |
|---|---|---|---|---|
| `collecting` | queued for a turn | facts; `complete` and the gate: windows and `ready_to_decide`, or a refusal and `rejected` | the turn is replayed, after the ceiling `manual_review` | row `sent`, same status |
| `ready_to_decide` | queued for a turn | `book_slot`: booking and `meeting_scheduled`, a busy window gives new windows; a fact correction recomputes the gate | same | same |
| `meeting_scheduled` | queued for a turn | knowledge base answers; `book_slot` and `offer_slots` are rejected; reschedule: `escalate`, the manager gets in touch, a reschedule task, `manual_review` | same | same |
| `rejected`, `stopped`, `manual_review` | saved, the bot stays silent | none | none | the last reply is `sent` |
| `refused` | to `collecting`, facts intact | none | none | the farewell is `sent` |

From a live status `refuse` leads to `refused`, `stop_contact` to `stopped`, and the booking is cancelled. Closing into `rejected`, `refused` and `stopped` writes a CRM note with the reason; a cancelled booking adds a task for the manager. If the lead goes silent there are no follow-up touches, the case waits.

`manual_review` is a ready reply to the lead plus a CRM task, and the case is frozen. Reasons: `escalate`, exhausted guard regenerations (`guard`), `critic_regen_limit`, the attempt ceiling, a closed booking boundary in dialog (`meeting_blocked:*`).

A model failure is an OpenRouter error or a technical turn outcome: `invalid_facts` (two broken `save_facts` in a row), `critic_error`, `critic_budget`, `empty_content`, `loop_limit`. Such a turn is deferred by 1, 2 and 4 minutes and the input stays unprocessed; the next claim after three deferrals sends the case to `manual_review`. A failure on the gateway side (`ExternalFailure.ProviderSide`: outage, rate limit, empty balance) is different: it is not what the model wrote, so the turn keeps being deferred, with the backoff capped at 16 minutes, until the lead's oldest unanswered message is 24 hours old by the database clock. Only then the case goes to a human.

## Agent turn

Go owns the loop: up to 6 tool rounds and 9 model calls per turn, counting the critic and regenerations. One tool per round, the extra ones get an error.

| Tool | What it does |
|---|---|
| `search_kb` | knowledge base sections by names from the table of contents; `faq.md` is issued in advance |
| `save_facts` | a facts delta; values are validated before the snapshot is applied |
| `complete` | the end of collection: niche verdict, gate and decision in the tool answer |
| `offer_slots` | show windows; the lead's time window sets the selection bounds |
| `book_slot` | book the named time, `HH:MM` |
| `escalate` | to a human: `user_request`, `reschedule`, `aggression`, `other` |
| `refuse` | the lead's refusal, the case goes to `refused` |
| `stop_contact` | a request not to write, the case goes to `stopped` |

A completion cut by the token cap (`finish_reason: length`) is not a reply: it is dropped, the call repeats once, and a second stump is the technical outcome `empty_content`. Lines of a single hyphen next to real text are removed.

Every text draft passes the claim check, then the guard, then the critic. The guard allows up to two regenerations, the critic also up to two. Where the code has its own ready text (windows, refusal), a rejected reply is simply dropped and the lead gets the code's text. Otherwise the case goes to a human.

### Claim grounding

A draft can claim something about state that only the code changes. A registry in `agent/claims.go` maps a claim class to a lexicon and a grounding predicate:

| Class | Claim | Grounded when |
|---|---|---|
| C1 | "booked you for Thursday", "we are waiting for you at 12" | a live booking exists and every time token of the sentence matches it |
| C2 | "moved", "cancelled" | never: the code answers a move with its own text |
| C3 | "the manager is on the line", "will write to you" | never: a manager gets a case only through a handoff outcome |
| C8 | "I will come back on the 20th" | never: there is no scheduler |

A draft with an ungrounded claim goes back once with a remark (the tools stay available, so a model that wanted to book can call `book_slot`). If the claim survives, the sentence is cut. A question is not a claim, and a negation or a condition near the verb lets a sentence pass. If nothing is left: with a live booking the lead gets `meeting_alive` filled from the database; with an open booking boundary the code shows the windows of the day the lead named; otherwise the case goes to a human (`claim_dropped`). The cut classes are in the decision audit.

### Gate outcomes

| Outcome | When | What next |
|---|---|---|
| `offer` | the niche is a target or unconfirmed, revenue is not below the segment threshold | windows |
| `mixed_niche` | both a target direction and a refusal reason are named | windows, the task is marked "несколько ниш" (several niches) |
| `off_profile` | a refusal reason without a target direction | polite refusal, `rejected` |
| `too_early` | the upper bound of revenue is below the segment threshold | polite refusal, `rejected` |

## Boundaries

What the system does not do for any input. Each boundary has a test.

- An amount in a price context with an unknown tariff, and a price of another catalog tariff, never reach the lead.
- Stop names with cases, phones and e-mail, links outside the catalog, and retelling of the acceptance threshold and service mechanics are cut.
- `offer_slots` and `book_slot` before the niche verdict, before the full set of facts, with a closed gate and in `meeting_scheduled` are rejected, including when an injection demands them.
- A role does not infer `is_owner`: only the lead's direct answer makes them the owner.
- A reply never says that a meeting is booked, moved or cancelled, that a manager is on the line or that the bot will return on a date, unless the database or the turn outcome backs it.
- A reply cut off by the token cap never reaches the lead or the critic.
- After `stop_contact` the bot does not write for any incoming message.
- In `rejected` and `manual_review` an incoming message does not wake the case.

## Packages

| Package | Contents | Imports |
|---|---|---|
| `config` | environment and catalog, validation at start | stdlib |
| `qualify` | `Case`, `DecisionInput`, facts, tariffs, niche, gate | `config` |
| `knowledge`, `crm`, `calendar` | the base and its validation; CRM and a fake; windows | `config` |
| `agent` | turn loop, OpenRouter, guard, critic, prompts | `qualify`, `knowledge` |
| `store` | cases, messages, decisions, bookings, outbox | `qualify` |
| `dialog` | handler: claim, turn, decision, delivery | all of the above |
| `console` | demo transport | `dialog` |
| `eval` | golden fixtures and evaluation | `agent`, `qualify` |

Domain packages know nothing about the transport: the console reaches `dialog` through the `Sender` interface, the CRM through the `crm.CRM` interface.

## Decisions

- **The code decides, the model brings facts.** The gate, prices and booking admission are deterministic, so the same facts give the same outcome and it can be covered by a test. The prompt sets the tone, not the acceptance rules.
- **The turn is checked twice.** The agent answers the model with a refusal reason so that it continues the conversation. dialog re-checks the same boundary before booking: the turn result is untrusted, and a stub or a failure must not produce a meeting that bypasses the gate.
- **Booking in the decision transaction, uniqueness in the database.** The calendar only builds windows. Two turns for one window are resolved by the (manager, start) index: the loser gets fresh windows, not a confirmation of a booking that does not exist.
- **A window grid from the start of the working day with a step of the meeting duration.** Windows of one manager within a day do not overlap, so uniqueness by start is enough. The duration is at least 15 minutes: otherwise hundreds of windows go into the model context.
- **The manager with the earliest window, on a tie the smaller priority, then id.** The lead chooses a time, not a person.
- **A tariff threshold is inclusive, the price is by the lower bound of revenue, the gate by the upper bound.** The price is conservative, the gate lets doubtful leads through to a meeting.
- **A refusal on revenue closes the case.** Waiting for growth would need deferred touches, and the project has none.
- **The niche is a separate call, not a `save_facts` field.** The dialog model edits facts every turn and would rewrite the code to fit the conversation. An unclear classifier answer gives an unconfirmed profile and a meeting, not a refusal.
- **The B2B facts are fixed in code, everything else is in the catalog.** A fact schema from data would give nothing, while a time zone, persona gender, prices and texts in code would tie it to one customer.
- **Packages by domain.** Each package owns one boundary and is tested separately.
- **JSON config on the standard library**, one migration, applied at start.
- **A claim is checked against state, not against the model's honesty.** Prompt text asks the model not to promise things, but a rule the model can break needs a check that sees the result. The lexicon is narrow on purpose (a booking verb plus a time token, a manager verb plus a contact verb), so a fact such as "записала: Москва" is not a claim. Rejected: the critic alone, which sees the draft but not the database.
- **A provider outage is not a handoff.** The ceiling of three attempts is for output the model keeps getting wrong; an outage of the gateway is waited out for 24 hours, because a handoff of every conversation in ten minutes of downtime answers worse than a delay.
- **A bare "ок" after a booking is code.** A model turn on it tends to offer other windows or make promises; a fixed whitelist of confirmation words and a catalog text cannot.
- **Outbox for both the lead and the CRM.** An outward action is delivered after the commit, a retry carries the row key, and there will be no second CRM record.
