# relay

Lab relay (plan 002). This step: config, SQLite schema, namespace discovery, `GET /healthz`.

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

Discovery lists namespaces with label `lab.protopia.tech/target=true`. The owner is the name prefix before the first `-` (`p01-demo` → `p01`) when it is a known participant id; other names are logged once and ignored. A namespace that is no longer listed (label removed or namespace deleted) is marked disconnected; when it is listed again it reconnects. A failed list keeps the stored state.

The image runs as 65532 from `scratch`. A volume mounted on `/data` needs `securityContext.fsGroup: 65532` so the relay can write the database.

## Local run against the cluster

```sh
kubectl proxy --port=8001 &
go build -o /tmp/relay . && cd /tmp && \
PARTICIPANTS_FILE=./participants.txt ADMIN_KEY_FILE=./admin-key WEBHOOK_SECRET_FILE=./webhook-secret \
DB_PATH=./relay.db KUBE_API=http://127.0.0.1:8001 ./relay
```

`kubectl proxy` uses your kubeconfig, so no `KUBE_TOKEN` is needed.
