# Memory & Personalization Layer — Spec

A shared personalization service that gives AI clients (a web chat frontend and a
local coding agent) a persistent, cross-session picture of **the user** and **each
project**. Heavy synthesis runs centrally on the server once uploads go quiet;
lightweight writes can happen anytime. Storage is markdown subject files on disk, so
state stays inspectable and editable.

---

## 1. Goals & non-goals

**Goals**
- One store, many clients. The web frontend and the coding agent read/write the same memory.
- Subject kinds with different lifecycles: a **global user profile**, **per-project**
  areas, cross-cutting **topics**, and **people**.
- Two write paths: **batch consolidation** (extract + synthesize, centrally) and
  **direct insertion** by agents at any time.
- Everything is inspectable and user-editable. The user is the highest authority.
- Language-agnostic HTTP API; store is simple enough to reason about by hand.

**Non-goals (v1)**
- Codebase/semantic code search. That stays a *separate* retrieval concern (see §9). This layer is about the *user* and *project conventions/decisions*, not "where is function X."
- Multi-user/team sharing. Single-user first; nothing precludes a user dimension later.
- Being a general vector DB. Semantic query is optional and additive (§6.5).

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
     │   MCP: get_context / memory_*                                  │
     │   HTTP: /context  /memory  /ingest  /consolidate               │
     └───────────────┬───────────────────────────────────────────────┘
                     │
           ┌─────────▼──────────┐        ┌──────────────────────────┐
           │  Store             │◄───────│ Consolidation Pipeline   │
           │  subjects + facts  │        │ (quiet-timer, LLM work)  │
           └────────────────────┘        └──────────────────────────┘
```

- **Two front doors, one store (§6):** **MCP** for live agent use (read context, edit
  subjects as tools) and **HTTP** for the frontend, the inspection/edit UI, and bulk log
  upload. Both hit the same subjects and store.
- **Clients never extract.** They read a context bundle at session start, optionally edit
  subjects directly (MCP `memory_write` / HTTP `PUT`), and write their session logs to
  disk as they already do.
- **Collection is edge→center.** Because the store is central, session logs are gathered at
  the edge and uploaded (§10). This is dumb transport — no client-side LLM.
- **The pipeline** is where the expensive LLM work lives — it extracts facts from
  uploaded sessions and synthesizes subject bodies from their facts. It runs on the
  server automatically once uploads go quiet (§6.4).

---

## 3. Storage model

Two representations over the same data: **markdown subject files on disk** (the
human-facing artifact) and **SQLite machinery** (what the pipeline reasons over).

### 3.1 Subject files (the human-facing, editable one)

Memory is a folder of markdown files, one per **subject**, with YAML-ish frontmatter
plus a prose body of bullet lines:

```
memory/
  profile.md          # singleton: durable identity
  areas/crush.md      # one per project / ongoing thing
  topics/photography.md
  people/aunt-may.md
```

```ts
type Subject = {
  name: string;        // slug, unique within its kind; also the path stem
  kind: "profile" | "area" | "topic" | "people";
  description: string; // one-line retrieval key, shown in the listing
  aliases?: string[];  // other names the subject goes by (routing aid)
  projectId?: string;  // areas only: link to the project registry (§4.1)
  repos?: string[];    // the subject's git remotes, normalized
  body: string;        // markdown bullets, each tagged [stated] / [observed] / [inferred]
  version: string;     // content hash, for optimistic concurrency on writes
};
```

This is the inspectable, user-editable surface. The user is the highest authority:
synthesis revises bodies in place and is told to respect content it didn't write.

### 3.2 Facts and pipeline state (the atomic unit the daemon reasons over)

A **fact** is one durable observation extracted from a session's user turns, persisted
so synthesis never has to re-extract. Facts group by `(subjectKind, subjectName)`;
synthesis folds a subject's facts into its body.

```ts
type Fact = {
  id: number;
  source: string;        // "crush", ... — provenance
  sessionId: string;     // which session it came from
  subjectKind, subjectName: string;
  text: string;          // the durable statement, in natural language
  tag: "stated" | "observed" | "inferred";
  sensitivity?: string;  // non-empty if gated (§7); dropped before storage
  sessionDate: string;
};
```

SQLite holds the machinery, all of it derived or checkpointable:
- `sessions` / `turns` — the uploaded corpus (upserted, so re-uploads are idempotent).
- `facts` — durable extraction output.
- `extracted(source, sessionId)` — which sessions have been through extraction.
- `subjects` — an index over the markdown files (frontmatter fields plus
  `synth_max_fact_id`, the synthesis watermark: a subject is dirty when it has facts
  newer than the last id folded into its body).
- `projects` / `project_aliases` — the registry (§4.1).

Design notes:
- **Checkpoints at both phases.** Extraction marks sessions; synthesis carries a
  per-subject fact-id watermark. A crash or re-run resumes cleanly and never pays for
  the same LLM work twice.
- **The body is the document; facts are the ledger.** Re-synthesis revises the body
  rather than appending, which is what supersedes stale facts: the model replaces the
  old statement instead of keeping both. There is no separate reconcile pass.

---

## 4. Subjects & routing

Four kinds of subject, picked by the **nature** of the fact, not where it was said:

- **`profile`** — singleton. Durable identity only: name, role, employer, education,
  location, contact, pronouns. Deliberately small.
- **`areas/<slug>`** — one per project or ongoing thing. What the project is, its
  conventions, decisions, architecture.
- **`topics/<slug>`** — cross-cutting domains spanning projects (e.g. `frc-robotics`,
  `photography`). Durable preferences and skills that aren't project-specific.
- **`people/<slug>`** — one per person.

**Routing happens at extraction time.** The extractor sees the existing subject listing
(names, descriptions, aliases) and always routes a fact to an existing subject when one
fits, rather than inventing a near-duplicate. A fact like "uses App Router" is an area
fact and must NOT leak into the profile — otherwise the profile gets polluted with
things only true in one repo. A general preference voiced mid-coding-session routes to
profile or a topic. Origin ≠ destination.

The coding agent mostly contributes `areas/*` and `topics/*`; the web chat mostly
contributes `profile` and `people/*`; both read the merged memory.

### 4.1 Project identity — binding sessions to a project reliably

Linking an area to a project depends on independent clients, on different machines,
agreeing on what the project *is*. The naive key — **filesystem path — is the worst
choice**: clones, git worktrees, dir renames, and a second laptop each fragment one
logical project into several, and the web frontend has no path at all. Bind on stable,
portable signals instead, resolved centrally.

Each client sends a **bundle of identity hints** (best-effort, strongest first); the
service canonicalizes them against a **project registry**. The wire never carries a raw
path as the id.

```ts
type ProjectHints = {
  gitRemote?: string; // normalized origin remote (github.com/org/repo) — strong, portable
  path?: string;      // absolute workspace path — machine-local, weakest, tiebreaker only
  name?: string;      // human label; how the web frontend refers to a project
};
```

**Resolution (service, in order):**
1. `gitRemote` matches a registered alias → that project. Normalize hard: strip
   scheme/user/`.git`/trailing slash, lowercase host, and canonicalize ssh↔https so
   `git@github.com:o/r` ≡ `https://github.com/o/r`.
2. else `name`/`path` matches an alias → that project.
3. else → **new project**: mint a canonical id (uuid), seed the registry with every
   hint as an alias, return it.

Every resolution also **binds newly seen hints as aliases** on the matched project, so
later sessions resolve from more signals.

**The registry** maps many aliases → one canonical project, and is the seam for
everything natural keys can't reach:
```
project: { id, displayName, aliases: { remotes[], paths[], names[] } }
```
- **Web frontend → repo.** No path/remote, so the user picks from `displayName`s (or a
  chat that cites a repo URL supplies a `gitRemote` hint). Selection records a `name`
  alias.
- **Many remotes, one project** (canonical repo plus mirrors) → the area's `repos` list
  holds all of them, and each is registered as an alias of the same project, so
  resolving by any one mirror finds the same id.
- **Forks** (same code, different remote) → distinct projects by default; merge only if
  the user wants shared memory. Never auto-merge on shared history.

**Registry endpoints** (HTTP):
```
POST /projects/resolve       { hints }         → { projectId }
GET  /projects                                 → list (ids, displayNames, aliases)
POST /projects/{id}/aliases  { kind, value }   → bind another alias
```

Resolution happens at ingest/context time; an area's `projectId` always holds the
**canonical id**, never a raw path — so the link survives clones, renames, and machine
changes. A possible later addition: an explicit committed marker file (e.g.
`.memory-project`) as the strongest hint, since it travels with the repo.

---

## 5. The consolidation pipeline

The core. Runs on the server, triggered automatically once uploads go quiet (§6.4) or
on demand via `/consolidate`. Two checkpointed phases: **extract** facts from each
pending session, then **synthesize** each dirty subject from its facts.

### 5.1 Collect (drain user turns)
Sessions arrive via `/ingest` and are upserted into `sessions`/`turns`. Adapters run
**edge-side** (§10.1): each collector filters to **user turns** and normalizes them
into the common turn shape *before* upload, so the pipeline holds zero format knowledge
and only ever sees clean user turns.

Assistant turns are **not uploaded** — extraction ignores them anyway (§5.2), and from
a coding agent they're the bulk of the bytes (tool calls, diffs, code).
*(Coreference caveat: user turns like "go with the second one" need the preceding
assistant turn to resolve. If that loss bites, include a hard-truncated
preceding-assistant turn as context — tool payloads stripped, text capped. Later
refinement, not v1.)*

### 5.2 Extract (one LLM call per session)
Input: the session's user turns, the project it happened in (if resolved), and the
current subject listing so facts route to existing subjects instead of spawning
duplicates. Output: routed candidate facts.

The hard skill here isn't "pull facts" — it's telling **durable** facts about the user
apart from **ephemeral task chatter**. A coding session is mostly task-local ("this
test is flaky", "rename foo to bar") that must *not* become memory. This distinction is
the single biggest quality lever in the whole system; everything downstream just
prunes.

```ts
type Candidate = {
  text: string;            // the durable statement: "prefers Go for backend services"
  subjectKind: "profile" | "area" | "topic" | "people";
  subjectName: string;     // slug of the target subject
  description?: string;    // one-liner, only if this creates a new subject
  aliases?: string[];      // synonyms, only if new
  tag: "stated";           // these are the user's own words
  sensitivity: string | null;  // non-null if it touches a gated category (§7)
};
```

Extraction rules (the system-prompt backbone):
- Only facts about the **user** or the **project**, grounded in the user's own words.
  Never about the assistant.
- **Durable only** — if it's true only for this task or this hour, skip it.
- One clear statement per fact; group tightly-related details rather than splitting
  hairs.
- Calibrate to evidence: one mention → "mentioned X once", not "X expert".
- **Tag sensitivity, don't silently self-censor** — the gate drops it deterministically
  (§5.3), which keeps the decision auditable.

### 5.3 Gate & persist (deterministic — no LLM)
A cheap, model-free pass over candidates:

1. **Sensitivity drop.** Discard any candidate whose `sensitivity` is on the blocklist
   (§7). Gone before it ever touches the store.
2. **Slug & route.** Subject names are normalized to slugs; profile always routes to
   the singleton.
3. **Ensure subjects.** A fact targeting a subject that doesn't exist yet creates a
   placeholder file (frontmatter only) so it appears in the listing immediately. If the
   candidate's area *is* the session's own project, the area is linked to the registry
   id; areas merely mentioned in passing stay unlinked.
4. **Persist.** Facts are written and the session marked extracted, atomically.

Facts persist even when synthesis hasn't run yet — the expensive LLM work survives
crashes and re-runs, and synthesis never re-extracts.

### 5.4 Synthesize (one LLM call per dirty subject)
A subject is dirty when it has facts newer than its `synth_max_fact_id` watermark. For
each dirty subject (in parallel across subjects):

Input: the subject's kind, description, **current body**, and all its facts
(chronological). Output: a rewritten body.

This is a **revise-in-place**, not an append log: the model merges related facts into
coherent bullets, replaces stale statements when new facts supersede them ("now uses
Helix" over "uses Vim"), and is told to respect prior content it didn't write — user
edits survive synthesis. Every bullet keeps a `[stated]` / `[observed]` / `[inferred]`
provenance tag. The new body is written with the max fact id as the new watermark.

Supersession here is editorial rather than structural: instead of explicit
`supersedes` edges between records, the model rewrites the sentence. Thin subjects
whose synthesis comes back blank fall back to their facts rendered verbatim.

### 5.5 Idempotency & cost
- **Both phases are checkpointed.** Extraction marks sessions; synthesis carries the
  per-subject watermark. A crash mid-run loses at most the in-flight LLM calls.
- **Two LLM call shapes, both cheap-model jobs** (deepseek-flash-class): one extract
  per session, one synthesize per dirty subject. Nothing else calls the model.

---

## 6. Interfaces

The service exposes **two front doors over the same store**:

- **MCP** — for live agent use. An agent (Crush, etc.) speaks MCP to read context and
  edit subjects as it works. Ergonomic, tool-shaped, low-ceremony.
- **HTTP** — for everything heavier: the web frontend, the inspection/edit UI, bulk log
  upload from the edge, and any integration that isn't an MCP client.

Same subjects, same store underneath. MCP is a thin ergonomic wrapper over the read +
write paths; HTTP is the full surface.

### 6.1 MCP surface (agents, live)
```
tool get_context(project? | hints)  → injection bundle { profile, listing, area }
tool memory_list()                  → the subject listing (path, kind, description, aliases)
tool memory_read(path)              → a subject's body + version token
tool memory_write(path, body, …)    → create or fully overwrite (version-checked)
tool memory_append(path, line)      → add one fact without resending the body
tool memory_delete(path)            → remove a subject
```
`memory_write`/`memory_append` are the **arbitrary insertion** paths in tool form: an
agent mid-session writes a fact without waiting for the next consolidation pass. The
version token from `memory_read` makes concurrent edits safe — a mismatched write is
refused rather than clobbering.

### 6.2 HTTP — Context (read path — the injection bundle)
```
GET /context?project=<id>            (or ?gitRemote=&path=&name= hints)
  → { profile: <body>, listing: [...], area: <body>, projectId }
```
Client calls this at session start. Returns the small, high-value stuff to inject
upfront: the profile in full, the subject listing (so the client can decide which other
subjects to read), and — for a project session — that project's area file.

### 6.3 HTTP — Memory (subject files)
```
GET    /memory                       → the subject listing
GET    /memory/{path}                → a subject's markdown body (or ?format=json)
PUT    /memory/{path}                → create or overwrite (version-checked)
POST   /memory/{path}                → append one line
DELETE /memory/{path}                → delete a subject
```
Paths are `profile`, `areas/<name>`, `topics/<name>`, `people/<name>`. Backs both the
inspection/edit UI and any non-MCP integration.

### 6.4 HTTP — Ingest & consolidate
```
POST /ingest        → upload normalized, role-tagged turns from an edge collector
POST /consolidate   → trigger a pipeline pass now
```
Because the store is **centrally hosted** (§2), turns are gathered and normalized at the
edge and uploaded — `/ingest` is that path. Its payload is the **common turn schema**
(role-tagged turns + hints + timestamps), not raw logs: adapters run edge-side (§10.1), so
raw transcripts never leave the machine and the center stays format-agnostic. This is dumb
transport — the LLM work still happens centrally in the pipeline (§5).

Consolidation is otherwise **automatic**: each ingest starts (or extends) a short quiet
timer, and a pass runs once uploads stop. Sessions arrive in bursts, so consolidating on
every ingest would burn API calls on a queue that is still filling; a max-wait cap keeps
a continuously-uploading machine from deferring forever.

### 6.5 (Optional, later) Semantic query
```
GET /query?q=   → subjects/facts ranked by semantic similarity
```
Only worth adding once the flat listing stops scaling (roughly a few hundred subjects).
Not needed for v1.

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
- **Origin ≠ destination.** `projectHints` says where the session *happened* (resolved
  to a canonical project id, §4.1), not where its facts *land*. A general preference voiced in
  a project session still routes to profile or a topic at extraction (§5.2). Wiring project-origin
  straight to fact-target scope would mean the profile never learns anything from coding
  sessions — a silent, costly bug.
- **Upload user turns only.** Extraction ignores assistant turns (§5.2), and from a coding
  agent they're most of the bytes — so the edge filters to `role: "user"` before upload. The
  `role` field stays in the schema for the optional
  coreference-context case (§5.1) and for non-agent sources that may want system turns; in
  the common path every uploaded turn is `user`. Secret redaction is still worth doing
  edge-side (§7).

### 6.7 Worked example: the Crush adapter

Crush persists to a local SQLite DB (`.crush/crush.db` in the workspace),
so this adapter is a **DB read**, not log parsing.

Mapping:
- **`projectHints`** ← computed from the workspace the `.crush` dir belongs to (§4.1):
  `git remote get-url origin` normalized (`gitRemote`) and the workspace root (`path`).
  Send all it can; the service resolves them to a canonical id. Do **not** send a bare
  path as the id.
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
- **Subject routing** — area facts never pollute the global profile.
- **Sensitivity blocklist** — certain inference categories are never persisted, dropped at
  §5.3 before storage. Decide the list explicitly; err toward not storing.
- **Anti-over-accommodation** — a memory that faithfully mirrors the user makes the agent
  sycophantic and can entrench bad patterns. Keep facts descriptive
  ("prefers terse answers"), never prescriptive instructions that would suppress honest
  feedback.
- **Provenance & authority** — every bullet carries its `[stated]` / `[observed]` /
  `[inferred]` tag, and synthesis is told to respect prior body content. The user is the
  highest authority: hand edits to a subject file survive synthesis.
- **Inspectable + editable** — the subject files ARE the control surface. Plain markdown
  on disk; edit with any tool, over MCP, or over HTTP. This doubles as the correction
  mechanism and the privacy escape hatch.

---

## 8. Injection strategy

Three tiers, by how much they change:
1. **Global profile** — small, loaded upfront every session.
2. **Project area file** — loaded upfront per project; the listing covers the rest.
3. **Codebase knowledge** — on-demand retrieval, NOT folded into memory (§9).

Keeps always-on context tight while the expensive/large stuff stays lazy.

---

## 9. Deliberately out of scope: codebase knowledge

"Where does function X live" is a retrieval problem over code, with different freshness and
indexing needs, and it goes stale the moment the repo changes. Keep it a separate system
the agent queries as a tool. Folding it into this layer bloats context and rots fast.

---

## 10. Build notes / open questions

- **Store backend** — markdown subject files on disk plus SQLite for the machinery
  (sessions, turns, facts, registry, checkpoints). Add a vector index only if/when §6.5
  is needed.
- **Contradiction surfacing** — synthesis resolves supersession editorially, but a
  genuine standing tension between two facts has no representation beyond both bullets
  coexisting. Whether that needs a UI is open.
- **Log adapters** — the coding agent and web frontend likely write logs in different
  formats; each needs an adapter that normalizes to a common turn shape and reliably tags
  role (the user-turns-only rule depends on it). You control the frontend format, so make it
  emit something clean.
- **Build vs buy for the pipeline** — extract → synthesize is a few hundred lines around
  two prompts plus the store. A hosted memory layer ships this, but native/in-process
  keeps it language-native.

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
A. If Crush speaks MCP to the service, `get_context` + `memory_write` already cover live
reads and live insertions with no session-end hook — so A's main advantage evaporates, while
B generalizes to every log-writing client. A is the middle option that gets squeezed: more
work than MCP for live, less general than the collector for bulk. Reach for A only if you
want same-session freshness that the quiet-timer pipeline won't give and MCP writes didn't
capture.

**Adapter placement: edge-side (decided).** The collector normalizes to the common turn
schema before upload, so raw transcripts never leave the machine and the service stays
format-agnostic — a clean data-minimization boundary for a self-hosted homelab. Accepted
cost: format knowledge is distributed across collectors, so a client changing its log format
means updating (and redeploying) that edge collector rather than fixing one spot centrally.
Consequence: the **common turn schema is now the edge↔center wire contract** and the first
thing worth pinning down.
```