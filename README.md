# lard

memory layer for my homelab; basically just a place to chuck data into from a bunch of different llm sources and then consolidate it

The canonical repo for this is hosted on tangled over at [`dunkirk.sh/lard`](https://tangled.org/dunkirk.sh/lard)

## Running it

```sh
# server: copy the example config, add your API key
cp config.example.toml ~/.config/lard/config.toml
# edit ~/.config/lard/config.toml, set llm.api_key
go run ./cmd/lard                    # listens on :7477

# client: point it at the server, then load everything you have
lard-client login                    # asks for the url, runs the device grant
lard-client backfill --root ~/code

# keep it fed in the background (macOS)
lard-client service install
```

## client

```
lard-client login    [--url URL] [--token TOKEN] [--root DIR...] [-f]
lard-client logout                        # revoke + forget credentials
lard-client status                        # server, auth, agent at a glance
lard-client backfill [--root DIR...]      # every session ever, idempotent
lard-client sync     [--workspace DIR...] # new sessions only
lard-client daemon   [--interval 5m]      # sync in a loop, for non-macOS init
lard-client service  install|uninstall|status
lard-client consolidate                   # force a pass now
```

## Interfaces

MCP at `POST /mcp`: `get_context`, `memory_list`, `memory_read`, `memory_write`, `memory_append`, `memory_delete`. add to crush:

```json
{
    "mcp": {
        "lard": {
            "type": "http",
            "url": "https://lard.your.domain/mcp",
            "oauth": true
        }
    }
}
```

HTTP

```
GET    /healthz                liveness check (no auth)
GET    /whoami                 verify credentials; returns the caller's identity
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

The server reads `~/.config/lard/config.toml` (override with `LARD_CONFIG`).
Every option can also be set as an environment variable; env always wins.
See `config.example.toml` for a ready-to-edit starting point.

| TOML key | env var | default | desc |
| --- | --- | --- | --- |
| `addr` | `LARD_ADDR` | `:7477` | listen address |
| `db` | `LARD_DB` | `~/.config/lard/lard.db` | sqlite path (sessions, facts, registry) |
| `memory_dir` | `LARD_MEMORY_DIR` | `~/.config/lard/memory` | subject files |
| `llm.base_url` | `LARD_HYPER_BASE_URL` | `https://hyper.charm.land` | OpenAI-compatible endpoint for consolidation |
| `llm.model` | `LARD_MODEL` | `deepseek-v4-flash` | consolidation model |
| `llm.api_key` | `LARD_HYPER_API_KEY` | | API key for consolidation (falls back to `HYPER_API_KEY`) |
| `auth.mode` | `LARD_AUTH` | `none` | `none` \| `token` \| `oauth` |
| `auth.token` | `LARD_TOKEN` | | shared secret for `token` mode |
| `auth.auth_server` | `LARD_AUTH_SERVER` | | authorization server URL for `oauth` mode |
| `auth.public_url` | `LARD_PUBLIC_URL` | | lard's external url; goes in the OAuth metadata |
| `auth.allowed_client_ids` | `LARD_OAUTH_CLIENT_IDS` | | comma list of client ids allowed to call lard |
| `auth.allowed_users` | `LARD_OAUTH_USERS` | | comma list of `me` urls allowed to call lard |
| `auth.required_scopes` | `LARD_OAUTH_SCOPES` | | comma list of scopes every token must carry |
| `collector.client_id` | `LARD_COLLECTOR_CLIENT_ID` | | OAuth client id collectors should use |
| `collector.scopes` | `LARD_COLLECTOR_SCOPES` | `profile offline_access` | scopes the collector should request (`offline_access` gets a refresh token) |
| `consolidate.after` | `LARD_CONSOLIDATE_AFTER` | `5m` | quiet period before a pass; `off` to disable |
| `consolidate.max_wait` | `LARD_CONSOLIDATE_MAX_WAIT` | `30m` | cap on that wait during constant uploads |

## auth

`token` mode is a shared secret,`oauth` mode makes lard an OAuth 2.1 protected resource in front of any authorization server that supports introspection ([rfc 7662]) and serves OAuth metadata ([rfc 8414]). lard serves
- `/.well-known/oauth-protected-resource` (rfc 9728) naming the authorization
  server
- `/.well-known/oauth-authorization-server` as a redirect to the authorization
  server, so older mcp clients still discover it. a redirect rather than a
  proxy because clients check that the issuer matches where they fetched the
  document

### login (device grant)

the only login flow is the OAuth device authorization grant ([rfc 8628]):

```
POST {as}/auth/device              client gets a device code + user code + url
GET  {as}/device?code=XXXX-XXXX    user approves, from any browser anywhere
POST {as}/auth/token               client polls until the token appears
```

requirements on the provider: it must serve rfc 8414 metadata advertising `device_authorization_endpoint` and support the device grant.

[rfc 8628]: https://datatracker.ietf.org/doc/html/rfc8628
[rfc 7662]: https://datatracker.ietf.org/doc/html/rfc7662
[rfc 8414]: https://datatracker.ietf.org/doc/html/rfc8414

<p align="center">
    <img src="https://raw.githubusercontent.com/taciturnaxolotl/carriage/main/.github/images/line-break.svg" />
</p>

<p align="center">
    <i><code>&copy; 2026-present <a href="https://dunkirk.sh">Kieran Klukas</a></code></i>
</p>

<p align="center">
    <a href="https://tangled.org/dunkirk.sh/lard/blob/main/LICENSE.md"><img src="https://img.shields.io/static/v1.svg?style=for-the-badge&label=License&message=MIT&logoColor=d9e0ee&colorA=363a4f&colorB=b7bdf8"/></a>
</p>
