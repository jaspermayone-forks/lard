# lard

memory layer for my homelab; basically just a place to chuck data into from a bunch of different llm sources and then consolidate it

The canonical repo for this is hosted on tangled over at [`dunkirk.sh/lard`](https://tangled.org/dunkirk.sh/lard)

## Run it

```sh
# server; consolidation needs a hyper API key
echo 'HYPER_API_KEY=sk-hyper-...' > .env
go run ./cmd/lard                    # listens on :7477

# client: backfill crush sessions
LARD_URL=http://localhost:7477 go run ./cmd/lard-client backfill --root ~/code

# update it
LARD_URL=http://localhost:7477 go run ./cmd/lard-client daemon --interval 5m

# run a consolidation pass
LARD_URL=http://localhost:7477 go run ./cmd/lard-client consolidate
```

## Interfaces

MCP (for agents) at `POST /mcp`: `get_context`, `memory_list`, `memory_read`, `memory_write`, `memory_append`, `memory_delete`. add to crush:

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

HTTP

```
GET    /context?project=<id>   profile + subject listing + this project's area
GET    /memory                 the subject listing
GET    /memory/{path}          a subject's markdown body
PUT    /memory/{path}          create or overwrite a subject
POST   /memory/{path}          append a line
DELETE /memory/{path}          delete a subject
POST   /ingest                 upload sessions
POST   /consolidate            trigger a pass
POST   /projects/resolve       hints → canonical project id
GET    /projects               project registry
```

paths are `profile`, `areas/<name>`, `topics/<name>`, `people/<name>`.

## Configuration

| var | default | desc |
| --- | --- | --- |
| `LARD_ADDR` | `:7477` | listen address |
| `LARD_DB` | `~/.config/lard/lard.db` | sqlite path (sessions, facts, registry) |
| `LARD_MEMORY_DIR` | `~/.config/lard/memory` | subject files |
| `LARD_AUTH` | `none` | `none` \| `token` \| `bearer` |
| `LARD_TOKEN` | | shared secret for `token` mode |
| `LARD_INDIKO_URL` | `https://indiko.dunkirk.sh` | auth server for `bearer` mode |
| `LARD_PUBLIC_URL` | | lard's external url; goes in the oauth metadata |
| `LARD_OAUTH_CLIENT_IDS` | | comma list of client ids allowed to call lard |
| `LARD_OAUTH_USERS` | | comma list of indiko `me` urls allowed to call lard |
| `LARD_OAUTH_SCOPES` | | comma list of scopes every token must carry |
| `HYPER_API_KEY` | | hyper API key for consolidation |
| `LARD_MODEL` | `deepseek-v4-flash` | consolidation model |

client env: `LARD_URL`, `LARD_TOKEN`.

## auth

`token` mode is a shared secret; good enough for the collector cron.

`bearer` mode makes lard an oauth 2.1 protected resource in front of
[indiko](https://indiko.dunkirk.sh). it serves

- `/.well-known/oauth-protected-resource` (rfc 9728) naming indiko as the
  authorization server
- `/.well-known/oauth-authorization-server` as a redirect to indiko, so older
  mcp clients still discover it. a redirect rather than a proxy because clients
  check that the issuer matches where they fetched the document

a `401` carries `WWW-Authenticate` with `resource_metadata`, which is enough for
an mcp client to find indiko and start the pkce flow on its own.

set `LARD_OAUTH_CLIENT_IDS` or `LARD_OAUTH_USERS`. indiko mints tokens for every
app you sign into, so without an allowlist any one of them can read all your
memory. lard warns at boot if you skip it.

indiko has no dynamic client registration, so mcp clients that insist on
`POST /register` will not connect. use a client that accepts a configured
client id. indiko also rejects a client id whose host differs from the redirect
uri host unless that url publishes `redirect_uris` metadata, so the simplest
working setup is a localhost client id matching a pinned callback port.

for crush, in `crush.json`:

```json
{
  "mcp": {
    "lard": {
      "type": "http",
      "url": "http://127.0.0.1:7477/mcp",
      "oauth_client_id": "http://localhost:40704/",
      "oauth_callback_port": 40704
    }
  }
}
```

then run lard with `LARD_AUTH=bearer`, `LARD_PUBLIC_URL` set to the same origin
crush dials, and `LARD_OAUTH_CLIENT_IDS=http://localhost:40704/`.

<p align="center">
    <img src="https://raw.githubusercontent.com/taciturnaxolotl/carriage/main/.github/images/line-break.svg" />
</p>

<p align="center">
    <i><code>&copy; 2026-present <a href="https://dunkirk.sh">Kieran Klukas</a></code></i>
</p>

<p align="center">
    <a href="https://tangled.org/dunkirk.sh/lard/blob/main/LICENSE.md"><img src="https://img.shields.io/static/v1.svg?style=for-the-badge&label=License&message=MIT&logoColor=d9e0ee&colorA=363a4f&colorB=b7bdf8"/></a>
</p>
