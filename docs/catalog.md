# Catalog and prompt evaluation

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

Needs an OpenRouter key. Every fixture from `data/example/golden` is run n times through a live model. The report shows the pass rate, unsafe outcomes and divergence classes. Runs that failed on transport are counted separately and make the command exit with an error. `-prompts` evaluates a candidate set of prompts against the same knowledge base, `-out` saves the report as JSON.

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
