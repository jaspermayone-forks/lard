# Memory & Personalization Layer — Spec

A shared personalization service that gives AI clients (a web chat frontend and a
local coding agent) a persistent, cross-session picture of **the user** and **each
project**. Heavy synthesis runs as a nightly/morning daemon; lightweight writes can
happen anytime. Storage is a path-keyed, KV-style document store so state stays
inspectable and editable.

---

## 1. Goals & non-goals

**Goals**
- One store, many clients. The web frontend and the coding agent read/write the same memory.
- Two scopes with different lifecycles: a **global user profile** and **per-project** context.
- Two write paths: a **batch consolidation daemon** (nightly) and **direct insertion** by agents at any time.
- Everything is inspectable and user-editable. The user is the highest authority.
- Language-agnostic HTTP API; store is simple enough to reason about by hand.

**Non-goals (v1)**
- Codebase/semantic code search. That stays a *separate* retrieval concern (see §9). This layer is about the *user* and *project conventions/decisions*, not "where is function X."
- Multi-user/team sharing. Single-user first; scope keys leave room to add a user dimension later.
- Being a general vector DB. Semantic query is optional and additive (§6.3).

---

## 2. Architecture

Central service in the homelab; clients and collectors reach it over the network.

```
     ┌──────────────┐                        ┌──────────────────┐
     │ web chat UI  │                        │ local coding     │
     │ (frontend)   │                        │ agent (Crush)    │
     └──────┬───────┘                        └───┬──────────┬───┘
            │ HTTP (context, memory, UI)         │ MCP      │ writes logs
            │                                     │ (live)   ▼
            │                                     │   ┌──────────────┐
            │                                     │   │ session logs │
            │                                     │   │  on disk     │
            │                                     │   └──────┬───────┘
            │                                     │          │ collected +
            │                                     │          │ uploaded
            ▼                                     ▼          ▼
     ┌───────────────────────────────────────────────────────────────┐
     │              Memory Service  (homelab, central)                │
     │   MCP: get_context / remember / forget                         │
     │   HTTP: /context  /memory/{ns}  /ingest  /consolidate          │
     └───────────────┬───────────────────────────────────────────────┘
                     │
           ┌─────────▼──────────┐        ┌──────────────────────────┐
           │  Store             │◄───────│ Consolidator Daemon      │
           │  records + docs    │        │ (cron: nightly, LLM work)│
           └────────────────────┘        └──────────────────────────┘
```

- **Two front doors, one store (§6):** **MCP** for live agent use (read context, write
  facts as tools) and **HTTP** for the frontend, the inspection/edit UI, and bulk log
  upload. Both hit the same records and reconciliation.
- **Clients never extract.** They read a context bundle at session start, optionally write
  facts directly (MCP `remember` / HTTP `PUT`), and write their session logs to disk as they
  already do.
- **Collection is edge→center.** Because the store is central, session logs are gathered at
  the edge and uploaded (§10). This is dumb transport — no client-side LLM.
- **The consolidator** is where the expensive LLM work lives — it processes uploaded logs,
  extracts and reconciles facts, and regenerates the rendered documents. Runs on the homelab
  box on a nightly cron.

---

## 3. Storage model

Two representations over the same data.

### 3.1 KV / document view (the human-facing, inspectable one)

Keys are namespaced paths; values are rendered documents. This is the view that "looks
like a KV store." It's what a settings UI renders and what gets injected into context.

```
profile/identity
profile/preferences
profile/patterns
project/<project-id>/conventions
project/<project-id>/decisions
project/<project-id>/session-log/<yyyy-mm-dd>
```

A document is regenerated from its underlying records by the daemon; direct writes can
also touch it. Markdown body is fine — it's readable and injects cleanly.

### 3.2 Record view (the atomic unit the daemon reasons over)

Every fact is a record. Documents are a projection of active records in a namespace.

```ts
type Scope =
  | { kind: "profile" }
  | { kind: "project"; projectId: string };

type Record = {
  id: string;                    // stable uuid
  scope: Scope;
  key: string;                   // groups records: "preferences.formatting", "conventions.build"
  value: string;                 // the fact, in natural language
  confidence: number;            // 0..1
  klass: "static" | "dynamic";   // stable identity vs recent context — different decay
  source: "batch" | "agent" | "user";  // provenance; user > batch > agent for authority
  status: "active" | "superseded" | "contradicted";
  supersedes?: string[];         // ids this record replaced
  contradicts?: string[];        // ids in unresolved tension (both kept)
  createdAt: string;
  updatedAt: string;
  lastSeenAt: string;            // last time evidence reinforced it (drives decay)
};
```

Design notes:
- **`source` is load-bearing.** `user` edits pin a record and win reconciliation.
  `agent` (direct) insertions are convenient but lower-trust and still get reconciled.
  `batch` is the daemon's own extraction.
- **`static` vs `dynamic`** so "senior engineer, prefers directness" doesn't churn at the
  same rate as "currently debugging the auth flow." Different decay curves (§5.6).
- **supersession vs contradiction are different edges.** Replacing an obsolete fact is not
  the same as two standing preferences in tension. Collapsing them is what makes profiles
  either go stale or thrash.

---

## 4. Scopes & routing

- **`profile/*`** — global, follows the user across every client and project. Role,
  durable preferences, communication style, recurring patterns.
- **`project/<id>/*`** — conventions, architectural decisions, past corrections, session
  logs. Isolated per project.

**Scope routing happens at extraction time.** A fact like "uses App Router" is
project-scoped and must NOT leak into the global profile — otherwise the profile gets
polluted with things only true in one repo. The extractor tags each candidate with a
scope; project-specific facts never land in `profile/*`.

The coding agent mostly contributes `project/*`; the web chat mostly contributes
`profile/*`; both read the merged profile.

### 4.1 Project identity — binding sessions to a project reliably

The whole `project/<id>/*` scope depends on independent clients, on different machines,
agreeing on what `<id>` is. The naive key — **filesystem path — is the worst choice**: clones,
git worktrees, dir renames, and a second laptop each fragment one logical project into
several, and the web frontend has no path at all. Bind on stable, portable signals instead,
resolved centrally.

Each client sends a **bundle of identity hints** (best-effort, strongest first); the service
canonicalizes them against a **project registry**. The wire never carries a raw path as the id.

```ts
type ProjectHints = {
  marker?: string;    // explicit id from a committed .memory-project file — STRONGEST
  gitRemote?: string; // normalized origin remote (github.com/org/repo) — strong, portable
  path?: string;      // absolute workspace path — machine-local, weakest, tiebreaker only
  name?: string;      // human label; how the web frontend refers to a project
};
```

**Resolution (service, in order):**
1. `marker` present → its id is canonical. Explicit and travels with the repo; nothing to infer.
2. else `gitRemote` matches a registered alias → that project. Normalize hard: strip
   scheme/user/`.git`/trailing slash, lowercase host, and canonicalize ssh↔https so
   `git@github.com:o/r` ≡ `https://github.com/o/r`.
3. else `name`/`path` matches an alias → that project.
4. else → **new project**: mint a canonical id, seed the registry with every hint as an alias,
   return it.

**Bootstrap fuzzy → exact.** On first resolution of a repo with no marker, write a
**committed** `.memory-project` back (id = canonical). First contact matches on the git remote;
every session after is an exact marker hit. Committed, not gitignored — travelling with clones
is the entire point.

**The registry** maps many aliases → one canonical project, and is the seam for everything
natural keys can't reach:
```
project: { id, displayName, aliases: { markers[], remotes[], paths[], names[] } }
```
- **Web frontend → repo.** No path/remote, so the user picks from `displayName`s (or a chat
  that cites a repo URL supplies a `gitRemote` hint). Selection records a `name` alias.
- **Many repos, one project** (service + client + infra) → the user **merges**: union their
  aliases under one id. A deliberate action, never inferred.
- **Forks** (same code, different remote) → distinct projects by default; merge only if the
  user wants shared memory. Never auto-merge on shared history.
- **Monorepo, many logical projects** → marker (or a path suffix under one remote) carries a
  sub-scope. Likely post-v1.

**Registry endpoints** (HTTP):
```
POST /projects/resolve       { hints }        → { projectId, created }
GET  /projects                                → list (ids, displayNames, aliases)
POST /projects/{id}/aliases  { alias }         → bind another alias
POST /projects/merge         { into, from }    → union aliases, repoint records
```

Resolution happens at collect/ingest time; the internal scope key `project:<id>` always uses
the **canonical id**, never a raw path — so records never fragment as clients, machines, and
paths shift under them.

---

## 5. The consolidation daemon (nightly)

The core. Runs on cron (nightly/morning), per user, per project touched since last run.

### 5.1 Collect (drain user turns)
Drain the turns uploaded via `/ingest` since the per-source watermark. Adapters run
**edge-side** (§10.1): each collector filters to **user turns** and normalizes them into the
common turn shape *before* upload, so the consolidator holds zero format knowledge and only
ever sees clean user turns. Group by scope and proceed.

Assistant turns are **not uploaded** — extraction ignores them anyway (§5.2), and from a
coding agent they're the bulk of the bytes (tool calls, diffs, code). The session-log doc is
sourced from agent-authored records instead (§5.5), not reconstructed from transcript.
*(Coreference caveat: user turns like "go with the second one" need the preceding assistant
turn to resolve. If that loss bites, include a hard-truncated preceding-assistant turn as
context — tool payloads stripped, text capped. Later refinement, not v1.)*

### 5.2 Extract observations (user turns only)
One LLM call per session (chunk only if a session is huge). Input: the session's user turns
+ origin scope. Output: atomic candidate facts.

The hard skill here isn't "pull facts" — it's telling **durable** facts about the user apart
from **ephemeral task chatter**. A coding session is mostly task-local ("this test is flaky",
"rename foo to bar") that must *not* become memory. This distinction is the single biggest
quality lever in the whole system; everything downstream just prunes.

Emit atomic candidates:
```ts
type Candidate = {
  observation: string;   // grounded: what the user actually said (short paraphrase)
  fact: string;          // the durable statement: "prefers Go for backend services"
  key: string;           // dotted category: "preferences.language", "conventions.pkg-manager"
  scopeHint: "profile" | "project";
  klass: "static" | "dynamic";
  confidence: number;    // 0..1 — how strongly this turn supports the fact
  sensitivity: string | null;  // non-null if it touches a gated category (§7)
  sourceTurn: number;    // provenance / show-your-work
};
```

Target ontology (a frame for the model; keys stay open but should cluster here):
- **profile / static** — identity & role; durable preferences (tooling, language, formatting,
  communication style).
- **profile / dynamic** — current focus, habits still stabilizing.
- **project / static** — conventions (package manager, style rules), standing corrections.
- **project / append** — decisions, dated: "decided <date>: <what>".

Extraction rules (the system-prompt backbone):
- Only facts about the **user** or the **project**, grounded in the user's own words. Never
  about the assistant or third parties.
- **Durable only** — if it's true only for this task or this hour, skip it.
- **Atomic** — one dimension per candidate; split compound statements.
- **High recall, low precision** — when unsure whether something's durable, emit it at low
  confidence and let the gate + reconcile prune (extract-first-filter-later).
- **Tag sensitivity, don't silently self-censor** — tag it so the gate can drop it *and* you
  can audit what was dropped.

### 5.3 Gate & route (deterministic — no LLM)
A cheap, model-free pass over candidates. Deterministic = auditable and free of model
variance, which is exactly what you want for the privacy and routing gates.

1. **Sensitivity drop.** Discard any candidate whose `sensitivity` is on the blocklist (§7).
   Gone before it ever touches the store.
2. **Scope routing** via a key-prefix table; `scopeHint` only breaks ties:
   ```
   identity.* preferences.* comms.* workflow.*   → profile   (even from a project session)
   conventions.* decisions.* corrections.*        → project
   ```
   The load-bearing rule from §6.6: a general preference voiced mid-coding-session routes
   **up** to profile. Origin ≠ destination. Wiring project-origin straight to project-scope is
   the silent bug that keeps the profile from ever learning from real work.
3. **Within-batch dedupe.** Collapse candidates that are the same fact from multiple turns
   (keep max confidence, earliest `sourceTurn`).
4. **Clamp** confidence to sane bounds.

### 5.4 Reconcile against existing records
Now each surviving candidate meets memory. Per candidate:

**a. Retrieve neighbors** — existing active records in the same `(scope, key)`. In flat v1
that's just the handful of records under that key. (Semantic retrieval only at §6.5 scale.)

**b. Classify** candidate vs neighbors (LLM; batched — one call handles all of a session's
candidates, each with its neighbor set):
- **NEW** — no neighbor covers this dimension → insert at candidate confidence.
- **REINFORCE** — a neighbor states the same value → don't duplicate; bump it and refresh
  `lastSeenAt`.  `c ← c + (1 − c)·α`  (e.g. α = 0.3), so confidence rises toward 1 with repetition.
- **SUPERSEDE** — a neighbor holds an *older value on the same single-valued dimension* →
  insert new; mark old `superseded`, add a `supersedes` edge, decay old `c ← c·β` (e.g. β =
  0.5). ("now uses Helix" over "uses Vim".)
- **CONTRADICT** — conflicts but isn't a clean replacement → keep both active, add a
  `contradicts` edge, nudge both confidences down, surface the pair for user resolution (§10).
  ("prefers terse" vs "prefers detailed walkthroughs".)

The **supersede-vs-contradict** call is the subtle one — the heuristic the classifier applies:
- single-valued dimension + newer evidence → **supersede** (people change editors, they don't
  hold two at once).
- multi-valued or context-dependent dimension, or conflicting evidence of similar recency →
  **contradict** (keep both; let the user or later evidence break the tie).

**Authority: `user` > `batch` > `agent`.** A candidate may **not** supersede a user-pinned
record — if it tries, drop or flag it, never overwrite the user. User edits always win.

**c. Apply** — writes, edges, confidence updates, version bump.

### 5.4a Idempotency & batching
- **Never process a session twice.** Reconcile mutates state (confidence bumps), so mark
  `(source, sessionId)` consolidated on success and skip already-done sessions on re-run.
  With idempotent ingest (§6.6), the whole pipeline is safe to re-run and crash-resume.
- **Two LLM calls per session** is the target: one extract, one reconcile-classify (all
  candidates + neighbor sets together). This is a cheap-but-capable-model job (Haiku-class),
  not a frontier-model one.

### 5.5 Merge / regenerate documents
Rebuild each namespace's rendered document from its active records (the running-summary
step). This is a revise-in-place operation: existing doc + new/changed records → updated
doc, not an ever-growing append log.

The **session-log doc** is a special case: it's fed by agent-authored records
(`source: "agent"`) that the live agent writes at session close via MCP `remember` — a tight
"did X, decided Y, next Z" — since the agent has the full session in-window and summarizes it
far better than the center could reconstruct from transcript. The consolidator just renders
those records like any other namespace; it doesn't rebuild the log from raw turns.

### 5.6 Decay & prune
Age out low-confidence `dynamic` records past a threshold. `static` records decay far more
slowly or not at all. `user`-sourced records don't auto-prune.

### 5.7 Commit
Write records + regenerated docs, bump versions, advance the per-source log watermark
(mtime / last-processed offset).

---

## 6. Interfaces

The service exposes **two front doors over the same store**:

- **MCP** — for live agent use. An agent (Crush, etc.) speaks MCP to read context and jot
  facts as it works. Ergonomic, tool-shaped, low-ceremony.
- **HTTP** — for everything heavier: the web frontend, the inspection/edit UI, bulk log
  upload from the edge, and any integration that isn't an MCP client.

Same records, same reconciliation, same docs underneath. MCP is a thin ergonomic wrapper
over the read + direct-insert paths; HTTP is the full surface.

### 6.1 MCP surface (agents, live)
```
tool get_context(project?)      → injection bundle { profile, project, sessionLog }
tool remember(scope, key, value)→ direct fact insertion (source: "agent")
tool forget(namespace, key)     → soft-delete
(later) tool search(scope, q)   → semantic lookup
```
`remember` is the **arbitrary insertion** path in tool form: an agent mid-session writes a
fact without waiting for the nightly pass. It still gets reconciled on the next run.

### 6.2 HTTP — Context (read path — the injection bundle)
```
GET /context?project=<id>
  → { profile: <doc>, project: <doc>, sessionLog: <recent doc> }
```
Client calls this at session start. Returns the small, high-value stuff to inject upfront:
global profile (static + high-confidence dynamic) + project doc + latest session log.

### 6.3 HTTP — Memory (KV read/write)
```
GET    /memory/{namespace}              → rendered document
GET    /memory/{namespace}/records      → structured records
PUT    /memory/{namespace}/{key}        → upsert one record   (direct insertion)
POST   /memory/{namespace}/records      → append observation(s)
DELETE /memory/{namespace}/{key}        → soft-delete / forget
```
Backs both the inspection/edit UI and any non-MCP integration.

### 6.4 HTTP — Ingest & consolidate
```
POST /ingest        → upload normalized, role-tagged turns from an edge collector
POST /consolidate   → trigger a daemon pass now (also runs on cron)
```
Because the store is **centrally hosted** (§2), turns are gathered and normalized at the
edge and uploaded — `/ingest` is that path. Its payload is the **common turn schema**
(role-tagged turns + scope + timestamps), not raw logs: adapters run edge-side (§10.1), so
raw transcripts never leave the machine and the center stays format-agnostic. This is dumb
transport — the LLM work still happens centrally in the consolidator (§5).

### 6.5 (Optional, later) Semantic query
```
GET /query?scope=&q=   → records ranked by semantic similarity
```
Only worth adding once flat retrieval stops scaling (roughly a few hundred records per
scope). Not needed for v1.

### 6.6 Common turn schema (the edge↔center wire contract)

Every collector normalizes to this before upload. It's the one interface everything else
keys off, so keep it small and stable.

```ts
// One normalized turn.
type Turn = {
  index: number;          // 0-based position within the session; ordering + idempotency
  role: "user" | "assistant" | "system" | "tool";
  content: string;        // flattened plain text; adapters strip tool scaffolding (§6.7)
  ts: string;             // ISO 8601 UTC; approximate from the session if per-turn missing
};

// One session's turns + its origin context.
type SessionBatch = {
  sessionId: string;      // STABLE id from the source (e.g. Crush's session id); idempotency key
  source: string;         // "crush" | "web-frontend" | ...; provenance + which adapter produced it
  projectHints?: ProjectHints;  // identity signals (§4.1); service resolves → canonical id. absent = general
  startedAt: string;      // ISO 8601 UTC
  endedAt?: string;       // ISO 8601 UTC; absent if still open / unknown
  turns: Turn[];
};

// The /ingest request envelope: one or more sessions.
type IngestRequest = {
  collector: string;      // uploading collector/host id; for debugging + per-source watermark
  sessions: SessionBatch[];
};
```

Three things that make or break this contract:

- **Idempotency.** The service upserts on `(source, sessionId)` and keys turns by `index`,
  so a collector that crashes mid-upload or re-sends an open session is safe — no dupes, no
  double-counted facts. This is what lets the edge be dumb and retry freely.
- **Origin scope ≠ fact scope.** `projectHints` says where the session *happened* (resolved
  to a canonical project id, §4.1), not where its facts *land*. A general preference voiced in
  a project session still routes up to the profile at extraction (§5.3). Wiring project-origin
  straight to fact-target scope would mean the profile never learns anything from coding
  sessions — a silent, costly bug.
- **Upload user turns only.** Extraction ignores assistant turns (§5.2), and from a coding
  agent they're most of the bytes — so the edge filters to `role: "user"` before upload. The
  session-log doc is agent-authored via `remember` (§5.5), not rebuilt from assistant turns,
  so the center never needs them. The `role` field stays in the schema for the optional
  coreference-context case (§5.1) and for non-agent sources that may want system turns; in
  the common path every uploaded turn is `user`. Secret redaction is still worth doing
  edge-side (§7).

### 6.7 Worked example: the Crush adapter

Crush persists to a local SQLite DB (`.crush/crush.db` by default; `crush dirs` to resolve),
so this adapter is a **DB read**, not log parsing. Column names below are illustrative — map
them to the real `internal/db/sql/` schema.

Mapping:
- **`projectHints`** ← computed from the workspace the `.crush` dir belongs to (§4.1): read
  `.memory-project` if present (`marker`), `git remote get-url origin` normalized
  (`gitRemote`), and the workspace root (`path`). Send all it can; the service resolves them to
  a canonical id. Do **not** send a bare path as the id.
- **`sessionId`** ← Crush's session id, verbatim (already stable — ideal idempotency key).
- **`turns`** ← the session's **user** messages, ordered; `index` = position among them,
  `ts` ← message `created_at`. Filter to user role in the query — assistant/tool messages
  don't get uploaded.
- **`content` flattening** — Crush user messages are usually plain text, so this is mostly a
  no-op; if a user turn carries attachments/pasted blocks, keep the text and drop binary
  scaffolding. (The heavy tool-call/result flattening only mattered when we were shipping
  assistant turns — now moot.)

```ts
// Pseudo — illustrative columns; map to internal/db/sql/.
async function crushAdapter(dbPath: string, workspace: string, since: number): Promise<SessionBatch[]> {
  const db = openSqlite(dbPath, { readonly: true });
  const projectHints = {
    marker:    readMarker(workspace),            // .memory-project if committed, else undefined
    gitRemote: normalizeRemote(gitOriginUrl(workspace)),  // ssh↔https canonicalized (§4.1)
    path:      workspace,                        // tiebreaker only
  };
  // Incremental: only sessions with messages newer than the last watermark for this DB.
  const sessions = db.query(
    `SELECT id, created_at, updated_at FROM sessions
     WHERE updated_at > ? ORDER BY created_at`, [since]);

  return sessions.map((s) => {
    const rows = db.query(
      `SELECT parts, created_at FROM messages
       WHERE session_id = ? AND role = 'user' ORDER BY created_at, id`, [s.id]);
    const turns: Turn[] = rows.map((m, i) => ({
      index: i,
      role: "user",
      content: flattenText(m.parts),         // usually plain text; strip binary/attachment scaffolding
      ts: toIso(m.created_at),
    }));
    return {
      sessionId: s.id, source: "crush", projectHints,
      startedAt: toIso(s.created_at), endedAt: toIso(s.updated_at),
      turns,
    };
  });
}
```

**Watermark:** track the max `updated_at` (or message rowid) seen per DB so re-runs only pick
up new/changed sessions; combined with the service's `(source, sessionId)` upsert, an
overlapping re-read is harmless. Open sessions get re-sent next run with more turns appended —
idempotency absorbs it.

**Locking:** open the DB **read-only** and be tolerant of Crush holding a write lock (it's had
SMB/lock issues); a co-located collector reading the local file is the happy path.

---

## 7. Safeguards

- **User-turns-only extraction** — prevents the agent profiling its own suggestions.
- **Scope routing** — project facts never pollute the global profile.
- **Sensitivity blocklist** — certain inference categories are never persisted, dropped at
  §5.3 before storage. Decide the list explicitly; err toward not storing.
- **Anti-over-accommodation** — a profile that faithfully mirrors the user makes the agent
  sycophantic and can entrench bad patterns. Keep the profile descriptive
  ("prefers terse answers"), not prescriptive ("always agree with the user"). Don't let the
  profile encode instructions that suppress honest feedback.
- **Provenance & authority** — `user` > `batch` > `agent`. User edits pin records and win
  reconciliation.
- **Inspectable + editable** — the KV view IS the control surface. Surface
  confidence-weighted records; let the user prune. This doubles as the correction mechanism
  and the privacy escape hatch.

---

## 8. Injection strategy

Three tiers, by how much they change:
1. **Global profile** — small, loaded upfront every session.
2. **Project doc** — loaded upfront per project.
3. **Codebase knowledge** — on-demand retrieval, NOT folded into the profile (§9).

Keeps always-on context tight while the expensive/large stuff stays lazy.

---

## 9. Deliberately out of scope: codebase knowledge

"Where does function X live" is a retrieval problem over code, with different freshness and
indexing needs, and it goes stale the moment the repo changes. Keep it a separate system
the agent queries as a tool. Folding it into this layer bloats context and rots fast.

---

## 10. Build notes / open questions

- **Store backend** — start with flat files or SQLite behind the API; both give the KV feel
  and are trivial to inspect. Add a vector index only if/when §6.5 is needed.
- **Watermarking** — per-source watermark so `/consolidate` is idempotent and resumable
  across partial uploads.
- **Conflict UI** — how to surface unresolved `contradicts` pairs to the user for a decision.
- **Log adapters** — the coding agent and web frontend likely write logs in different
  formats; each needs an adapter that normalizes to a common turn shape and reliably tags
  role (the user-turns-only rule depends on it). You control the frontend format, so make it
  emit something clean.
- **Build vs buy for the profile pipeline** — the extract → reconcile → merge chain is a
  few hundred lines around one extraction prompt plus the record store. A hosted memory
  layer ships this, but native/in-process keeps it language-native.
- **Session log growth** — date-stamped `session-log/<date>` docs; the daemon can roll old
  ones into a compressed summary.

### 10.1 Collection: how logs reach the central service

Central hosting means logs must travel edge→center. Two viable feeders (not exclusive):

- **A — Crush session-end push.** A hook in the agent POSTs the session's user turns (or
  full transcript) to `/ingest` when a session closes.
  - *Pros:* precise (you send exactly what you want), near-real-time, no log parsing, no
    separate process to run.
  - *Cons:* per-client work — every tool you want covered needs its own hook; only captures
    instrumented clients; couples you to Crush internals.
- **B — edge collector daemon.** A small process on each machine sweeps the day's session
  logs and uploads them to `/ingest` on a schedule.
  - *Pros:* client-agnostic — anything that writes logs is covered, including future tools
    and the web frontend; decoupled from client internals; one collector per machine.
  - *Cons:* needs the per-format adapters; batch latency (daily); its own thing to run.

**Recommendation:** lean on **MCP for the live path** and **B for the bulk path**, and skip
A. If Crush speaks MCP to the service, `get_context` + `remember` already cover live reads
and live insertions with no session-end hook — so A's main advantage evaporates, while B
generalizes to every log-writing client. A is the middle option that gets squeezed: more
work than MCP for live, less general than the collector for bulk. Reach for A only if you
want same-session freshness that the nightly collector won't give and MCP `remember` didn't
capture.

**Adapter placement: edge-side (decided).** The collector normalizes to the common turn
schema before upload, so raw transcripts never leave the machine and the service stays
format-agnostic — a clean data-minimization boundary for a self-hosted homelab. Accepted
cost: format knowledge is distributed across collectors, so a client changing its log format
means updating (and redeploying) that edge collector rather than fixing one spot centrally.
Consequence: the **common turn schema is now the edge↔center wire contract** and the first
thing worth pinning down.
```