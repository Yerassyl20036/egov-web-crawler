# Ask data.egov.kz

[![CI](https://github.com/Yerassyl20036/egov-web-crawler/actions/workflows/ci.yml/badge.svg)](https://github.com/Yerassyl20036/egov-web-crawler/actions/workflows/ci.yml)

**An AI agent that finds the right government dataset on [data.egov.kz](https://data.egov.kz), Kazakhstan's open-data portal. Ask in English, Russian or Kazakh and get an answer with verified links to the datasets.**

<!-- DEMO: replace this placeholder with a GIF or screenshot of a real session, e.g. ![demo](docs/demo.png) -->
> **Demo:** _screenshot coming soon._ Run it locally in two commands (see [Run it](#run-it)).

---

## In short

data.egov.kz publishes about **4,000 datasets**: hospitals, schools, budgets, land plots, tariffs and so on. Finding the right one is hard:

- titles and descriptions are in Russian and Kazakh only;
- the same dataset title is often published **by every region separately** (for example, "List of state hospitals by region" exists 15+ times, once per regional akimat);
- the portal's own search is plain keyword matching.

**Ask data.egov.kz** puts an LLM agent in front of the catalog. You describe what you need in your own words. The agent turns that into Russian search keywords, searches a Postgres full-text index of every dataset passport, checks the best candidates and answers with links. You can watch every step it takes in the UI. Each link is checked against the catalog, so a made-up link is flagged.

| | |
|---|---|
| **AI / agents** | Tool-using agent on Claude (Anthropic Go SDK): streaming, adaptive thinking, server-side refusal fallbacks, validated tool inputs, citation verification |
| **Retrieval (RAG)** | Postgres full-text search with Russian stemming + `pg_trgm` fuzzy matching, coverage-based ranking for multi-region duplicates |
| **Evaluation** | 39-question multilingual eval set with dev/holdout split; retrieval metrics run in CI without an API key, agent-level eval on demand |
| **Backend** | Go 1.26: HTTP API with Server-Sent Events, crawler with worker pool and retries, embedded SQL migrations |
| **Frontend** | React 19 + TypeScript + Vite chat UI showing tool steps and clickable citations; works on mobile |
| **Infra** | Docker multi-stage builds, Docker Compose, GitHub Actions (vet, race tests against Postgres, frontend build, evals, image builds) |

## Architecture

```mermaid
flowchart LR
    subgraph Browser
        UI[React chat UI<br/>steps + citations]
    end
    subgraph Go API
        HTTP["/api/chat (SSE)<br/>/api/search, /api/health"]
        AGENT[Agent loop<br/>internal/agent]
        TOOLS[Tools:<br/>search_datasets<br/>get_dataset<br/>list_categories<br/>preview_rows*]
    end
    PG[(Postgres 16<br/>FTS russian/simple/english<br/>+ pg_trgm)]
    CLAUDE[[Claude API<br/>claude-opus-5-5]]
    PORTAL[(data.egov.kz)]
    CRAWLER[cmd/crawler] -->|listing JSON + passports| PORTAL
    CRAWLER -->|upsert| PG
    SEED[cmd/seed] -->|snapshot| PG
    UI -->|question| HTTP
    HTTP --> AGENT
    AGENT <-->|streamed turns| CLAUDE
    AGENT --> TOOLS
    TOOLS --> PG
    TOOLS -.->|*with EGOV_API_KEY| PORTAL
    HTTP -->|tool_call, tool_result, text, done| UI
```

### How one question is answered

1. The UI posts the conversation to `POST /api/chat` and reads the Server-Sent Events stream.
2. The agent streams a turn from Claude. The system prompt tells it to **search with Russian keywords even when the user writes in English or Kazakh**, to retry with synonyms or filters when results are weak, and to link only datasets a tool returned.
3. When Claude calls tools, the server validates each input (inputs stream eagerly, so nothing is trusted), runs the calls in parallel against Postgres and sends the results back. Every call and result is also pushed to the UI as a step.
4. When Claude answers, every `data.egov.kz/datasets/view?index=…` link is resolved against the catalog. Links that don't exist are marked as unverified in the UI.

| Tool | What it does |
|---|---|
| `search_datasets(query, category?, agency?, limit?)` | Hybrid full-text + trigram search over titles, keywords, agencies, categories and descriptions in ru/kk/en |
| `get_dataset(api_uri)` | The full passport: multilingual titles and descriptions, publisher, owner, update cadence, dates, popularity |
| `list_categories()` | The 19 portal categories with dataset counts, for filtering |
| `preview_rows(api_uri, limit?)` | Sample rows from the official data API. Only offered when `EGOV_API_KEY` is set |

**Model settings** (all overridable by env var): `claude-opus-5-5`, adaptive thinking with progress updates shown as steps, effort `medium`, up to 8 tool-loop steps per question. **Server-side refusal fallbacks are on by default** (`fallbacks: "default"`): if a safety classifier declines a request, the API retries it on the model Anthropic recommends for that refusal category. Set `AGENT_FALLBACKS=false` to turn this off.

### Search ranking

Titles repeat verbatim across regions, and the region usually appears only in the publishing agency ("Акимат Павлодарской области"). Plain `ts_rank` rewards repeated terms, so a copy that mentions "unemployment statistics" three times beat the copy from the region the user asked about. The index therefore has two generated `tsvector` columns:

- `head_vec`: title (weight A), keywords and publishing agency (B), category (D)
- `body_vec`: descriptions

The score is mainly **coverage**: the share of distinct query terms found in `head_vec`. `ts_rank`, description coverage, trigram title similarity (for typos) and popularity only break ties. Russian text goes through the Russian snowball stemmer and Kazakh through the `simple` config, because Postgres has no Kazakh stemmer. Prefix matching (`больниц:*`) covers the rest of the morphology.

## Evaluation

[`evals/cases.json`](evals/cases.json) has 39 questions (18 English, 11 Russian, 10 Kazakh), each labelled with the dataset(s) that answer it. Most are region-specific on purpose, because that is where naive search fails. 29 questions (**dev**) were used while tuning the ranking. 10 (**holdout**) were written afterwards and never tuned on.

### Retrieval: `search_datasets` alone (deterministic, runs in CI)

Each question is searched twice: with the Russian keyword query the agent is instructed to produce, and with the user's raw question (what a naive integration would send).

| Query mode / split | N | Hit@1 | Hit@5 | Recall@10 | MRR@10 |
|---|---:|---:|---:|---:|---:|
| Russian keywords: all | 39 | 0.95 | 1.00 | 1.00 | 0.97 |
| Russian keywords: dev | 29 | 0.97 | 1.00 | 1.00 | 0.98 |
| Russian keywords: **holdout** | 10 | **0.90** | **1.00** | 1.00 | 0.95 |
| Raw question: English | 18 | 0.28 | 0.33 | 0.31 | 0.31 |
| Raw question: Kazakh | 10 | 0.80 | 0.90 | 0.90 | 0.83 |
| Raw question: Russian | 11 | 1.00 | 1.00 | 1.00 | 1.00 |

What this shows:

- With good Russian keywords, the right dataset is in the top 5 for every question, including the holdout set.
- Raw English questions only find the right dataset a third of the time. Rewriting the query into Russian is the agent's main job, and the system prompt says so explicitly.
- The first version of the ranking (one weighted `tsvector`, plain `ts_rank`) scored Hit@1 0.76 / Hit@5 0.86 on the dev set. It lost mostly on region-specific questions. The eval made that measurable, and coverage ranking brought it to 0.97 / 1.00. The holdout set (0.90 / 1.00) was scored once, after tuning.

### Agent: end to end

`go run ./cmd/eval -agent` asks the agent every question and records whether the answer **cites an expected dataset**, whether any tool call **retrieved** one, how many links were **unverified** (not in the catalog), plus tool calls, latency and tokens.

<!-- AGENT_EVAL_RESULTS -->
_Agent results will be added after the first run with an API key (`go run ./cmd/eval -agent`). In CI it runs via **Actions → CI → Run workflow → agent_eval**, using the `ANTHROPIC_API_KEY` repository secret._

## Run it

### With Docker (recommended)

```bash
cp .env.example .env        # optionally add ANTHROPIC_API_KEY
docker compose up --build
```

- Chat UI: <http://localhost:3000>
- API: <http://localhost:8080/api/health>

Postgres starts, a one-shot `seed` container loads the bundled snapshot ([`data/datasets.jsonl.gz`](data/datasets.jsonl.gz), 3,947 datasets crawled on 2026-10-29), then the API starts. **Without `ANTHROPIC_API_KEY`** the app runs in search-only mode: the UI says so and questions go straight to full-text search.

### Without Docker

Requires Go 1.26+, Node 22.12+ and a Postgres 16 database.

```bash
export DATABASE_URL="postgres://egov:egov@localhost:5432/egov?sslmode=disable"
go run ./cmd/seed                       # load the snapshot
ANTHROPIC_API_KEY=sk-ant-... go run ./cmd/api   # :8080
cd web && npm install && npm run dev    # :5173, proxies /api to :8080
```

### Refresh the data from the portal

```bash
go run ./cmd/crawler -export data/datasets.jsonl.gz   # snapshot only (about 8 min, polite delays)
DATABASE_URL=... go run ./cmd/crawler                 # straight into Postgres
DATABASE_URL=... go run ./cmd/crawler -passports      # also scrape each HTML passport for the owner field
```

### Tests and evals

```bash
go test ./...                                         # unit tests, incl. the agent loop against a fake Messages API
TEST_DATABASE_URL=postgres://... go test ./internal/store   # search integration test
DATABASE_URL=... go run ./cmd/eval -v                 # retrieval eval, no API key needed
DATABASE_URL=... ANTHROPIC_API_KEY=... go run ./cmd/eval -agent   # end-to-end agent eval (costs tokens)
```

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | (required) | Postgres connection string |
| `ANTHROPIC_API_KEY` | | Enables the agent; search-only without it |
| `AGENT_MODEL` | `claude-opus-5-5` | Claude model id |
| `AGENT_EFFORT` | `medium` | `low` / `medium` / `high` / `xhigh` / `max` |
| `AGENT_THINKING_DISPLAY` | `updates` | `updates` (progress notes shown as steps), `summarized` or `omitted` |
| `AGENT_FALLBACKS` | `true` | Server-side refusal fallbacks |
| `AGENT_MAX_TOKENS` / `AGENT_MAX_TURNS` | `16000` / `8` | Per-response output cap / tool-loop steps per question |
| `EGOV_API_KEY` | | data.egov.kz API key; enables `preview_rows` |
| `CHAT_RATE_PER_MIN` | `10` | Chat requests per client IP per minute |
| `CHAT_DAILY_LIMIT` | `0` (off) | Chat requests per UTC day across all visitors; set it on a public demo |
| `WEB_DIR` | `/app/web` in the image | Serve the built UI from the API (single-container deploys) |
| `AUTO_SEED_FILE` | | Seed this snapshot on start if the catalog is empty |

## Project layout

```
cmd/
  api/        HTTP server: chat (SSE), search, health, static UI
  crawler/    crawls data.egov.kz into Postgres and/or a JSONL snapshot
  seed/       loads a snapshot or the legacy CSV into Postgres
  eval/       retrieval + agent evals, markdown report for CI
internal/
  agent/      Claude tool-use loop, tool definitions, system prompt
  api/        handlers, SSE streaming, rate limiting
  catalog/    dataset model, snapshot/CSV readers
  egov/       portal client: listing JSON, passport HTML parser, data API
  eval/       metrics (Hit@k, Recall@k, MRR, citation checks)
  store/      Postgres store, migrations, hybrid search
web/          React + TypeScript + Vite chat UI (nginx image for Compose)
evals/        cases.json: the labelled question set
data/         datasets.jsonl.gz snapshot; datasets_export.csv (original Mongo export, Feb 2026)
```

## Design notes and trade-offs

- **Postgres FTS instead of a vector database.** The catalog is small (4k rows) and keyword-heavy (place names, agency names, document numbers like "№382"). Lexical search with stemming plus an LLM that rewrites the query gives precise, explainable results and needs no embedding pipeline. Embeddings would be the next step for vaguer questions ("data about quality of life").
- **The agent searches; it doesn't load the catalog into context.** Tool results are compact (title, agency, cadence, 300-character description) so a question costs a few thousand tokens, not the whole catalog.
- **Stateless chat.** The client resends the visible conversation (question and answer text) with each question. Earlier tool results are not replayed. That keeps the server stateless and the history append-only, and the cited links in earlier answers keep enough context for follow-ups.
- **Trust boundaries.** Tool inputs are decoded strictly (unknown fields rejected, `api_uri` pattern-checked). Tools never run on a refused or truncated turn. Answer links are verified against the catalog, and the data API key is redacted from logs.
- **Row data.** The portal's row API (`/api/v4/{index}/v1`) needs a free API key, so `preview_rows` is optional and the core experience works on metadata alone.

## Origins

This repo started as a Go crawler that scraped the catalog into MongoDB (`data/datasets_export.csv` is that export, February 2026). The crawler's concurrency design survives in `cmd/crawler`. The rewrite moved storage to Postgres, fixed the passport parser (the portal now serves Kazakh by default, so the Russian-only labels missed fields) and switched to the portal's listing JSON, which carries Kazakh and English titles, keywords and agencies.
