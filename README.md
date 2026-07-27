# lard

memory layer for my homelab; basically just a place to chuck data into from a bunch of different llm sources and then consolidate it

The canonical repo for this is hosted on tangled over at [`dunkirk.sh/lard`](https://tangled.org/dunkirk.sh/lard)

## Run it

```sh
# server; consolidation needs a hyper API key
echo 'HYPER_API_KEY=sk-hyper-...' > .env
go run ./cmd/lard                    # listens on :7477

# client: point it at the server, then load everything you have
lard-client login                    # asks for the url, opens your browser
lard-client backfill --root ~/code

# keep it fed in the background (macOS)
lard-client service install
```

`login` asks where the server lives, then runs a browser flow against whatever
auth server lard names, so there is no token to copy. It always prints the raw
authorization URL, so you can paste it into a browser on another machine. On a
headless box pass `--url` and `--token` and it never prompts.

when the server brokers the login (the default once `LARD_COLLECTOR_CLIENT_ID`
is set) the collector opens no ports at all. you get a url on the server and it
polls until you finish, so ssh, containers, and headless boxes all work the same
way with nothing to forward.

after that nothing needs poking: the agent syncs on an interval, and the server
consolidates itself once uploads go quiet.

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

config lives at `~/.config/lard/client.json` (mode 0600, holds the token).
`LARD_URL` and `LARD_TOKEN` override it.

`service install` writes a launchd agent that syncs on an interval and survives
reboots. It refuses to install if it cannot reach the server, since a silent
background failure is the worst outcome. Logs go to
`~/Library/Logs/lard-client.log`. Linux is not wired up yet: run
`lard-client daemon` under systemd.

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
| `LARD_COLLECTOR_CLIENT_ID` | | oauth client id collectors should use |
| `LARD_COLLECTOR_SCOPES` | `profile` | scopes the collector should request |
| `LARD_CONSOLIDATE_AFTER` | `5m` | quiet period before a pass; `off` to disable |
| `LARD_CONSOLIDATE_MAX_WAIT` | `30m` | cap on that wait during constant uploads |
| `HYPER_API_KEY` | | hyper API key for consolidation |
| `LARD_MODEL` | `deepseek-v4-flash` | consolidation model |

client env: `LARD_URL`, `LARD_TOKEN` (both override `~/.config/lard/client.json`).

consolidation is automatic: an ingest starts a quiet timer, and the pass runs
once uploads stop. bursts collapse into one pass, and a machine uploading
continuously still gets consolidated at `LARD_CONSOLIDATE_MAX_WAIT`.

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

### collector registration

a collector cannot invent its own client id: the auth server decides which
clients exist, and lard decides which it trusts, so a guessed id gets rejected.
set `LARD_COLLECTOR_CLIENT_ID` to a client id registered with your auth server
and lard publishes it at `/auth/collector`; `lard-client login` adopts it. that
id is then trusted automatically, so it does not also need to be in
`LARD_OAUTH_CLIENT_IDS`.

### login (device grant)

the only login flow is the oauth device authorization grant ([rfc 8628]), run
against the authorization server directly — lard is not involved beyond handing
the collector its client id:

```
POST {as}/auth/device              client gets a device code + user code + url
GET  {as}/device?code=XXXX-XXXX    user approves, from any browser anywhere
POST {as}/auth/token               client polls until the token appears
```

no listener, no port forward, and no browser on the collector's machine, so
ssh, containers, and headless boxes all work the same way. no client secret
either: the device code itself is the proof of possession, so the collector is
an ordinary public client and there is nothing sensitive to ship.

requirements on the provider: it must serve rfc 8414 metadata advertising
`device_authorization_endpoint` and support the device grant (indiko does).
login fails with a clear message otherwise.

with no collector registration configured (`LARD_COLLECTOR_CLIENT_ID` unset),
`lard-client login` fails and tells you to set it.

[rfc 8628]: https://datatracker.ietf.org/doc/html/rfc8628

### indiko notes

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
