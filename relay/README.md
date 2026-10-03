# relay

Lab relay (plan 002): config, SQLite schema, namespace discovery, the GitHub webhook, the JSON API, `GET /healthz`. The UI comes later (`ui/`).

## Config

| Env | Default | |
|---|---|---|
| `PARTICIPANTS_FILE` | `/etc/relay/participants.txt` | `<id>,<display name>: <api key>`, `#` comments; see `../participants.example.txt`. A duplicate id or key, or a malformed line, stops the start (exit 2). |
| `ADMIN_KEY_FILE` | `/etc/relay/secret/admin-key` | Secret file, must not be empty. |
| `WEBHOOK_SECRET_FILE` | `/etc/relay/secret/webhook-secret` | Secret file, must not be empty. |
| `DB_PATH` | `/data/relay.db` | SQLite; migrations run at start. |
| `PORT` | `8080` | |
| `TARGET_DOMAIN` | `lab.patoarchitekci.io` | |
| `DISCOVERY_INTERVAL` | `30s` | |
| `KUBE_API`, `KUBE_TOKEN` | | Local runs only: API URL and optional bearer token instead of the in-cluster ServiceAccount. |
| `RELAY_ALLOW_PRIVATE_TARGETS` | `false` | `true` lets webhook URLs use `http://` and loopback or private addresses. **Local tests only, never in the cluster.** |

Discovery lists namespaces with label `lab.protopia.tech/target=true`. The owner is the name prefix before the first `-` (`p01-demo` → `p01`) when it is a known participant id; other names are logged once and ignored. A namespace that is no longer listed (label removed or namespace deleted) is marked disconnected; when it is listed again it reconnects. A failed list keeps the stored state.

The image runs as 65532 from `scratch`. A volume mounted on `/data` needs `securityContext.fsGroup: 65532` so the relay can write the database.

## GitHub webhook: `POST /webhook/github`

- Body up to 1 MiB (413 above). `X-Hub-Signature-256` is checked with the shared webhook secret; a missing or wrong signature gets 401 and is stored as `bad_signature` in `github_events` (the admin view; the newest 1000 rows of every received request are kept).
- `ping` → 200. `deployment_status` → 202. Other events → 202, ignored.
- Routing: the namespace comes from `deployment_status.environment_url` (`https://<ns>.<TARGET_DOMAIN>/...`), then `deployment.payload.environment_url`, then the run name (`workflow_run.display_title`, format of `run-name` in `deploy.yml`: `deploy <ns> <version> break=<break>`). GitHub sends `queued` and `in_progress` statuses without `environment_url`, so the run name routes them. The owner is the name prefix when it is a known participant, whether the namespace is connected or not. No owner → 202, stored as `unrouted`, not forwarded.
- Forward: in the background, one attempt, 10 s, the same body bytes and headers `Content-Type`, `X-GitHub-Event`, `X-GitHub-Delivery`, `X-GitHub-Hook-ID`, `X-Hub-Signature-256`, `X-Hub-Signature`, `User-Agent`. Each attempt is a row in `deliveries` with the headers and body for replay.
- Journal: `success`, `failure` and `error` statuses add a `deploy` entry with version, run name and run URL taken from `workflow_run` in the payload (no GitHub API call), or `target_url` / `log_url` without a name when `workflow_run` is missing. The entry is written even when the owner has no webhook URL.
- Outbound guard (forward, test, replay): `https://` only; the dialed IP (after DNS, so rebinding cannot swap it) must not be loopback, private (RFC 1918, fc00::/7), link-local (169.254.0.0/16, fe80::/10), unspecified or multicast; redirects are not followed.

`testdata/` holds real deliveries from a temporary hook on this repo (secret `test-secret`): the exact body bytes and the request headers.

## API

`X-Api-Key` with a participant or the admin key. No key or an unknown key → 401; a participant key on an admin endpoint, or the admin key on a participant endpoint → 403. JSON in and out, errors as `{"error": "..."}`.

| Endpoint | Key | |
|---|---|---|
| `GET /api/me` | both | `{id, name, admin}` |
| `GET /api/namespaces` | participant | own namespaces with connected state |
| `GET /api/webhook` | participant | `{url, updated_at, secret}`: the shared secret, for copying |
| `PUT /api/webhook-url` | participant | `{"url": "https://..."}`; `""` removes it |
| `POST /api/webhook/test` | participant | signed `ping` to the own URL; returns the delivery |
| `POST /api/webhook/replay?last=N` | participant | N 1..20 (default 1): resends the last N GitHub deliveries with original headers and body, oldest first, in the background; returns `{queued}` |
| `GET /api/deliveries` | participant | newest 50, without headers and body |
| `GET /api/journal?since=&participant=` | both | participant: own entries (`participant` of someone else → 403); admin: all or one. `since` is RFC 3339 and matches `updated_at`, so a poller also sees patched entries. Newest first, max 500. |
| `GET /api/participants` | admin | id, name, connected namespaces, webhook URL, last delivery, last journal entry time |
| `POST /api/journal` | admin | `{namespace, type: deploy\|drift, message, command, undo, status: open\|fixed}`; status defaults to `open`; the owner comes from the namespace prefix |
| `PATCH /api/journal/{id}` | admin | `{status, message}` |

## Local run against the cluster

```sh
kubectl proxy --port=8001 &
go build -o /tmp/relay . && cd /tmp && \
PARTICIPANTS_FILE=./participants.txt ADMIN_KEY_FILE=./admin-key WEBHOOK_SECRET_FILE=./webhook-secret \
DB_PATH=./relay.db KUBE_API=http://127.0.0.1:8001 RELAY_ALLOW_PRIVATE_TARGETS=true ./relay
```

`kubectl proxy` uses your kubeconfig, so no `KUBE_TOKEN` is needed. `RELAY_ALLOW_PRIVATE_TARGETS=true` is only for a receiver on `127.0.0.1`.
