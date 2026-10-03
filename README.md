# ai-coding-devops-lab

Instructor repo for the "AI Coding for DevOps/Ops" lab: the AKS cluster, the shop services and the chaos runner.

## Lab cluster

1. Copy `.env.example` to `.env` and fill in the subscription and tenant IDs.
2. Run `infra/aks.sh`. It creates the resource group, the public IP, AKS, the GitHub Actions identity and the GitHub variables (Azure ids, `RESOURCE_GROUP`, `AKS_NAME`).
3. Run the `oidc-check` workflow: `gh workflow run oidc-check.yml`. Copy the printed `sub` into `GHA_OIDC_SUBJECT` in `.env`.
4. Run `infra/aks.sh` again. It creates the federated credential.
5. Check the login: `gh workflow run oidc-check.yml -f login=true`.
6. Set `ACME_EMAIL` in `.env` and run `infra/cluster.sh`. It installs Traefik (default `IngressClass` `traefik`, on the static public IP, HTTP redirects to HTTPS except ACME challenges), cert-manager and the `ClusterIssuer` `letsencrypt` (HTTP-01). It prints the Traefik IP.
7. Add the DNS record by hand in Cloudflare: `A *.lab.patoarchitekci.io` → the printed IP, DNS only (no proxy).

`infra/destroy.sh --cluster` deletes the AKS cluster only. `infra/destroy.sh --all` deletes the resource group after confirmation.

Never commit IDs, IPs or secrets. They live in `.env` (gitignored) and in GitHub variables.

## App

`app/` is one Go module and one image. `ROLE` selects the service: `orders`, `inventory`, `payments` or `loadgen`. `PORT` defaults to `8080`. Logs are JSON on stdout, one line per request; a request the client gave up on is logged with `status: 499` and `canceled: true`.

- `orders`: `POST /orders` calls `inventory` (`POST /reserve`) and `payments` (`POST /charge`) at `INVENTORY_URL` and `PAYMENTS_URL` with a 2 s timeout. It returns 201 with `versions` of all three services, 502 on an upstream error and 504 on an upstream timeout.
- `orders`, `inventory`, `payments`: `GET /version`, `GET /healthz`, `GET /readyz`, and `POST /chaos/crash|oom|unready|slow`. `CHAOS=<mode>` applies a mode at start. `slow` delays every response by 5 s, except `/healthz`, `/readyz` and `/chaos/*`.
- `loadgen`: every `LOADGEN_INTERVAL` (default `1s`) sends `POST /orders` to each target with a 5 s timeout and logs `target`, `status`, `latency_ms`, `versions` and `error`. Targets are `TARGETS` (comma-separated base URLs) or, when unset, `https://<namespace>.<TARGET_DOMAIN>` (default `lab.patoarchitekci.io`) for every namespace labelled `lab.protopia.tech/target=true`, listed every 30 s through the Kubernetes API with the pod's ServiceAccount. It serves `GET /healthz` and `GET /metrics` (Prometheus text format, standard library only): `loadgen_requests_total{target,code}` (`code` is the HTTP status or `error`), `loadgen_request_duration_seconds_sum{target}` and `_count{target}`, and `loadgen_target_up{target}` (1 when the last request returned 201, else 0; dropped when the target leaves discovery, counters stay). No chaos endpoints, and no `/readyz` because nothing depends on it being ready.

```sh
cd app && go vet ./... && go test ./...
scripts/scenario-local.sh   # builds, starts compose, waits for /readyz, checks every failure mode, then docker compose down
```

The scenario publishes `orders`, `inventory` and `payments` on `ORDERS_PORT`, `INVENTORY_PORT` and `PAYMENTS_PORT` (defaults 8080, 8081, 8082). To try it by hand, start the stack and wait for readiness before the first request:

```sh
docker compose up -d --build
for p in "${ORDERS_PORT:-8080}" "${INVENTORY_PORT:-8081}" "${PAYMENTS_PORT:-8082}"; do
  until curl -sf "localhost:$p/readyz" >/dev/null; do sleep 1; done
done
curl -X POST "localhost:${ORDERS_PORT:-8080}/orders"
docker compose logs -f loadgen
docker compose down
```

Every service has `mem_limit: 128m` (no swap), so `/chaos/oom` ends in an OOM kill of that container.

The version comes from the build: `docker build --build-arg VERSION=1.0.7 app` sets `main.version`.

## Build

`.github/workflows/build.yml` (a thin caller of the reusable `image.yml`, inputs `dir` and `image`) runs on push to `main` (paths `app/**` and the workflow) and on `workflow_dispatch`. It runs `go vet` and `go test`, then pushes `ghcr.io/protopiatech-labs/shop:1.0.<run_number>` for `linux/amd64` and `linux/arm64`, with the git SHA in `org.opencontainers.image.revision`. There is no `latest` tag. Tags are immutable: if the tag exists, the job fails before the build, so a rerun never overwrites an image. The package must be public so the cluster and participants can pull without login.

```sh
docker buildx imagetools inspect ghcr.io/protopiatech-labs/shop:1.0.<N>
```

## Deploy

`deploy/base/` is the healthy shop: Deployments and Services `orders`, `inventory` and `payments`, the ConfigMap `shop-config` (`INVENTORY_URL`, `PAYMENTS_URL`, read by `orders` through `envFrom`), the `ResourceQuota` `shop` (the only quota source) and the Ingress `orders` (paths `/orders` and `/version` only, `cert-manager.io/cluster-issuer: letsencrypt`, TLS). Pods run as 65532 with a read-only root filesystem and no capabilities; `readinessProbe` is `/readyz`, `livenessProbe` is `/healthz`. The strategy is `Recreate`, so a broken deploy replaces the healthy pod instead of waiting behind it.

`deploy/components/` has one kustomize component per breakage:

| `--break` | Change | What the cluster shows |
|---|---|---|
| `bad-image` | `orders` image tag `0.0.0-does-not-exist` | `ImagePullBackOff` |
| `oom` | `orders` `CHAOS=oom`, memory limit 32Mi | `OOMKilled`, then `CrashLoopBackOff` |
| `crash` | `orders` `CHAOS=crash` | `CrashLoopBackOff` |
| `missing-config` | `orders` `envFrom` ConfigMap `shop-config-missing` | `CreateContainerConfigError` |
| `quota` | `orders` requests 1 CPU (quota 200m) | `FailedCreate ... exceeded quota` events |
| `unready` | `orders` `CHAOS=unready` | pod not ready, no endpoints, 503 from Traefik |
| `dependency-down` | `payments` replicas 0 | `orders` returns 502 |
| `slow` | `inventory` `CHAOS=slow` | `orders` returns 504 |

The base sets every field a component changes (for example `CHAOS=""`, the memory limit, `replicas: 1`), so a deploy without `--break` brings the namespace back to healthy without `--prune`.

```sh
scripts/deploy.sh <namespace> --version 1.0.N [--break <type>] [--dry-run]
```

`--version` is required. The namespace must be a DNS label, not `default`, `kube-*`, `lab-*`, `cert-manager` or `traefik`, and either new or already labelled `lab.protopia.tech/target=true`. The script writes a temporary overlay under `deploy/` (Namespace with the label, Ingress host `<namespace>.lab.patoarchitekci.io`, image tag, component) and runs `kubectl apply -k`. `--dry-run` prints `kubectl kustomize` output and skips the cluster check. The tag must exist in GHCR; the script does not build images.

Chaos endpoints are internal: `kubectl -n <namespace> port-forward svc/orders 8080` and `curl -X POST localhost:8080/chaos/unready`.

`deploy/loadgen/` runs `loadgen` in `lab-loadgen` with a ServiceAccount bound to a ClusterRole that can only get, list and watch namespaces, and the in-cluster Service `loadgen` (port 8080) that the relay scrapes for `/metrics`:

```sh
scripts/deploy-loadgen.sh --version 1.0.N [--dry-run]
```

## Deploy workflow

`.github/workflows/deploy.yml` runs `scripts/deploy.sh` from GitHub Actions, so every deploy is a GitHub deployment in environment `lab` and sends `deployment_status` events:

```sh
gh workflow run deploy.yml -f namespace=<namespace> -f version=1.0.N -f break=none   # break: none or a deploy/components name
```

It logs in with the managed identity (OIDC, `azure/login`), gets the AKS kubeconfig and converts it with `kubelogin -l azurecli`, then runs `deploy.sh`. It builds no image. Runs for the same namespace queue (`concurrency: deploy-<namespace>`), they never cancel each other. With `break=none` the job waits for `kubectl rollout status` of `orders`, `inventory` and `payments` (90 s each) and fails if the shop does not become ready. With a breakage it does not wait: unhealthy pods are the expected result, so the job succeeds once `kubectl apply` succeeds. A namespace that `deploy.sh` refuses fails the job.

## Chaos runner

The chaos runner lives in the instructor's private repo.

## Participants

`scripts/participants.sh` gives every participant access to the cluster. It reads a participants file, one line per participant, `<id>,<display name>: <relay API key>` (ids `p01`..`p99`; `#` comments and blank lines are skipped). The real file is not in git; see `participants.example.txt`.

```sh
scripts/participants.sh --participants participants.txt [--cards <dir>] [--vap-mode warn|deny]
scripts/participants.sh --participants participants.txt --delete   # asks for "yes"
```

`deploy/participants/` holds what it applies:

- `clusterrole-portal.yaml`: ClusterRole `lab-portal`: namespaces (create, get, list, watch, patch, update, delete), `resourcequotas` and `limitranges` (create, update, patch, get, list, delete), read on `deployments`, `replicasets`, `pods` and `events`. No Secrets, no ConfigMaps.
- `vap.yaml`: ValidatingAdmissionPolicy `lab-portal-prefix` and its binding. It matches only `system:serviceaccount:pXX-portal:portal` and allows writes to namespaces, quotas and limit ranges only in `pXX-*`, never in its own `pXX-portal`. The file is committed with `validationActions: [Deny]`; `--vap-mode warn` (testing only) applies `[Warn, Audit]` instead. The instructor, `deploy.yml`, loadgen and the relay are not matched. In `warn` mode the portal can still delete any namespace it can reach.
- `participant.yaml`: the template for one id (`${ID}`): namespace `pXX-portal` (label `lab.protopia.tech/participant=pXX`), ServiceAccount `portal` bound to `lab-portal`, ServiceAccount `deployer` with `admin` in `pXX-portal` only, ResourceQuota `portal` (`requests.cpu` 300m, `requests.memory` 512Mi, `limits.memory` 1Gi, `pods` 6), LimitRange `portal` (defaults for containers without resources), long-lived token Secrets for both accounts, and namespace `pXX-demo` labelled `lab.protopia.tech/target=true`, so `deploy.sh` accepts it.

Everything goes through `kubectl apply`, so a second run prints `unchanged`. For each id the script writes `<cards>/<id>/`: `portal.kubeconfig`, `deployer.kubeconfig` (server and CA from the current context, token from the Secret) and `card.md` (name, relay API key, hosts `pXX.lab.patoarchitekci.io` and `pXX-demo.lab.patoarchitekci.io`, namespaces). The default `--cards` is `../training-ai-coding-devops/instructor/cards` (the private training repo). Tokens are never printed. `--delete` removes `pXX-portal`, `pXX-demo` and the binding of each listed id; namespaces the portal created (`pXX-*`) and the cards stay.

## Discord

`scripts/discord-webhooks.sh` creates one Discord webhook `lab-<id>` per participant on one channel (Discord API v10). It needs `DISCORD_BOT_TOKEN` and `DISCORD_CHANNEL_ID` in `.env`; the bot needs View Channel and Manage Webhooks on the channel. The participants file is the same as for `participants.sh` (only the ids are used).

```sh
scripts/discord-webhooks.sh --participants participants.txt --out discord.txt
scripts/discord-webhooks.sh --participants participants.txt --delete   # asks for "yes"
```

The channel's webhooks are listed once; missing `lab-<id>` ones are created, existing ones are reused, so a second run creates nothing. `--out` gets one line per participant, `<id> https://discord.com/api/webhooks/<id>/<token>` (mode 600). URLs are never printed to the terminal; treat the file as a secret (anyone with a URL can post to the channel). HTTP 429 is retried after `retry_after`; any other error stops the script with the Discord message. `--delete` removes `lab-<id>` for the listed ids.

## Relay

`relay/` is the lab relay (see `relay/README.md`). `.github/workflows/relay-build.yml` calls the same `image.yml` for paths `relay/**` (its own run numbers): `go vet` and `go test`, then `ghcr.io/protopiatech-labs/relay:1.0.<run_number>` for `linux/amd64` and `linux/arm64`, immutable tags, no `latest`. The package must be public (`https://github.com/orgs/ProtopiaTech-Labs/packages/container/relay/settings`).

`deploy/relay/` runs it in `lab-relay`: ServiceAccount `relay` with a read-only ClusterRole (namespaces, deployments, pods), PVC `relay-data` (1Gi, `managed-csi`) on `/data`, one replica with `Recreate`, `fsGroup: 65532`, read-only root filesystem, Service and Ingress `relay.lab.patoarchitekci.io` with a Let's Encrypt certificate. The App column comes from `http://loadgen.lab-loadgen.svc:8080/metrics` (plain HTTP inside the cluster; no RBAC, no NetworkPolicy in the lab).

```sh
scripts/deploy-relay.sh --version 1.0.<N> [--dry-run]
scripts/github-webhook.sh
```

`deploy-relay.sh` writes ConfigMap `relay-participants` from `participants.txt` (repo root, not in git) and Secret `relay-admin` (`admin-key`, `webhook-secret`) from `RELAY_ADMIN_KEY` and `GITHUB_WEBHOOK_SECRET` in `.env`, applies `deploy/relay` with the tag and waits for the rollout. A checksum of that content is a pod template annotation, so a change restarts the pod and a rerun with the same input changes nothing. `github-webhook.sh` creates or updates the `deployment_status` hook of `GITHUB_REPO` pointing at `https://relay.lab.patoarchitekci.io/webhook/github` (JSON, the shared secret) and prints its id. Secrets go through files and stdin, never through arguments or the terminal.

```sh
curl https://relay.lab.patoarchitekci.io/healthz
kubectl -n lab-relay get secret relay-admin -o jsonpath='{.data.admin-key}' | base64 -d   # admin key
gh api repos/ProtopiaTech-Labs/ai-coding-devops-lab/hooks/<id>/deliveries --jq '.[] | {event, status_code}'
```
