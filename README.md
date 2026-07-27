# lard

A shared memory and personalization layer for AI clients. One central service
gives every agent (a coding agent like Crush, a web chat frontend, whatever
comes next) a persistent, cross-session picture of **you** and **each
project**. Heavy synthesis runs as a batch consolidation pass; lightweight
writes happen anytime. Storage is a path-keyed, inspectable document store
over SQLite.

The canonical repo for this is hosted on tangled over at [`dunkirk.sh/lard`](https://tangled.org/dunkirk.sh/lard)

Two binaries:

- **`lard`** — the central service (homelab). HTTP API + remote MCP over one
  SQLite store.
- **`lard-client`** — the edge collector. Reads Crush session logs, uploads
  normalized user turns. Two modes: `backfill` (scan everything ever) and
  `daemon` (periodic incremental sync).

## Quick start

```sh
# server (needs a hyper API key for consolidation)
echo 'HYPER_API_KEY=sk-hyper-...' > .env
go run ./cmd/lard                    # listens on :7477

# client: backfill every crush session on this machine
LARD_URL=http://localhost:7477 go run ./cmd/lard-client backfill --root ~/code

# then keep it fresh
LARD_URL=http://localhost:7477 go run ./cmd/lard-client daemon --interval 5m

# kick a consolidation pass (also runs server-side on your cron)
LARD_URL=http://localhost:7477 go run ./cmd/lard-client consolidate
```

## How it fits together

```
 crush (any machine)                web chat
   │ writes .crush/crush.db           │
   ▼                                  │
 lard-client (edge collector)         │
   │ user turns only, HTTP /ingest    │
   ▼                                  ▼
 ┌───────────────── lard (central) ─────────────────┐
 │  MCP: get_context / remember / forget            │
 │  HTTP: /context /memory/* /ingest /consolidate   │
 │        /projects/*  /conflicts                   │
 └───────────────┬──────────────────────────────────┘
                 ▼
        SQLite store: records + rendered docs
                 ▲
        consolidation pass (LLM, hyper):
        extract → gate → reconcile → render → decay
```

- **Records** are the atomic unit: one fact, with provenance
  (`user` > `batch` > `agent`), confidence, class (`static` vs `dynamic`),
  and supersede/contradict edges.
- **Documents** are rendered projections of active records in a namespace
  (`profile/preferences`, `project/<id>/conventions`, ...). This is the
  human-facing KV view and what gets injected into context.
- **Scope routing** happens at extraction: `identity.* preferences.* comms.*
  workflow.*` route to the global profile even when voiced in a project
  session; `conventions.* decisions.* corrections.*` stay in the project.
- **Project identity** resolves from hints (normalized git remote > name >
  path) against a server-side registry. Remotes are portable across machines
  and clones; paths and names are weaker tiebreakers. Forks stay distinct
  unless you merge them explicitly.

## Interfaces

### MCP (agents, live) — `POST /mcp` (streamable HTTP)

- `get_context(project?)` → `{ profile, project, sessionLog }` injection bundle
- `remember(scope, key, value)` → direct insertion, `source: agent`
- `forget(namespace, key)` → soft-delete

Add to crush:

```json
{
  "mcp": {
    "lard": {
      "type": "http",
      "url": "https://lard.your.domain/mcp"
    }
  }
}
```

### HTTP

```
GET    /context?project=<id>          injection bundle
GET    /memory/{namespace}            rendered markdown doc
GET    /memory/{namespace}/records    structured records
PUT    /memory/{namespace}/{key}      upsert (source: user — always wins)
POST   /memory/{namespace}/records    append observations (source: agent)
DELETE /memory/{namespace}/{key}      soft-delete
POST   /ingest                        upload normalized sessions
POST   /consolidate                   trigger a pass now
POST   /projects/resolve              hints → canonical id (remote/name/path)
GET    /projects                      registry
POST   /projects/{id}/aliases         bind an alias
POST   /projects/merge                union two projects
GET    /conflicts                     unresolved contradiction pairs
GET    /healthz
```

## Configuration

Server env (plus `.env` in the working directory):

| var | default | what |
| --- | --- | --- |
| `LARD_ADDR` | `:7477` | listen address |
| `LARD_DB` | `~/.config/lard/lard.db` | SQLite path |
| `LARD_AUTH` | `none` | `none` \| `token` \| `bearer` |
| `LARD_TOKEN` | | shared secret for `token` mode |
| `LARD_INDIKO_URL` | `https://indiko.dunkirk.sh` | auth server for `bearer` mode |
| `HYPER_API_KEY` | | hyper API key (consolidation) |
| `LARD_MODEL` | `deepseek-v4-flash` | consolidation model |
| `LARD_HYPER_BASE_URL` | `https://hyper.charm.land` | model API |

Client env: `LARD_URL`, `LARD_TOKEN`.

### Auth modes

- `none` — homelab trust.
- `token` — one shared bearer secret; right for the collector path.
- `bearer` — validate tokens against [indiko](https://indiko.dunkirk.sh)
  introspection, and proxy its OAuth metadata at
  `/.well-known/oauth-authorization-server` so MCP clients can discover it.

## Client modes

```sh
lard-client backfill --root ~/code --root ~/code/charm [--dry-run]
lard-client sync [--workspace .]            # incremental, watermark-driven
lard-client daemon [--root ~/code] [--interval 5m]
lard-client consolidate                     # trigger server pass
```

The collector reads `.crush/crush.db` read-only, filters to **user turns**
(assistant/tool bytes never leave the machine), normalizes to the common turn
schema, and uploads. Server-side `(source, sessionId)` upsert makes re-runs
and crash-resumes safe; watermarks only advance after a successful upload.

## Notes

- Consolidation is two LLM calls per session (extract, reconcile-classify)
  on a cheap model — `deepseek-v4-flash` by default.
- Contradictions surface at `GET /conflicts` for user resolution; the user
  is the highest authority and `PUT` pins a record forever.
- Codebase knowledge ("where is function X") is deliberately out of scope —
  that's a retrieval problem, not memory. See spec.md.

<p align="center">
    <img src="https://raw.githubusercontent.com/taciturnaxolotl/carriage/main/.github/images/line-break.svg" />
</p>

<p align="center">
    <i><code>&copy; 2026-present <a href="https://dunkirk.sh">Kieran Klukas</a></code></i>
</p>

<p align="center">
    <a href="https://tangled.org/dunkirk.sh/lard/blob/main/LICENSE.md"><img src="https://img.shields.io/static/v1.svg?style=for-the-badge&label=License&message=MIT&logoColor=d9e0ee&colorA=363a4f&colorB=b7bdf8"/></a>
</p>
