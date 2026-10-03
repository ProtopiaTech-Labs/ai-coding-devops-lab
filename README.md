# ai-coding-devops-lab

Instructor repo for the "AI Coding for DevOps/Ops" lab: the AKS cluster, the shop services and the chaos runner.

## Lab cluster

1. Copy `.env.example` to `.env` and fill in the subscription and tenant IDs.
2. Run `infra/aks.sh`. It creates the resource group, the public IP, AKS, the GitHub Actions identity and the GitHub variables.
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
- `loadgen`: every `LOADGEN_INTERVAL` (default `1s`) sends `POST /orders` to each target with a 5 s timeout and logs `target`, `status`, `latency_ms`, `versions` and `error`. Targets are `TARGETS` (comma-separated base URLs) or, when unset, `https://<namespace>.<TARGET_DOMAIN>` (default `lab.patoarchitekci.io`) for every namespace labelled `lab.protopia.tech/target=true`, listed every 30 s through the Kubernetes API with the pod's ServiceAccount. It serves `GET /healthz` only: no chaos endpoints, and no `/readyz` because nothing sends traffic to it.

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

`.github/workflows/build.yml` runs on push to `main` (paths `app/**` and the workflow) and on `workflow_dispatch`. It runs `go vet` and `go test`, then pushes `ghcr.io/protopiatech-labs/shop:1.0.<run_number>` for `linux/amd64` and `linux/arm64`, with the git SHA in `org.opencontainers.image.revision`. There is no `latest` tag. Tags are immutable: if the tag exists, the job fails before the build, so a rerun never overwrites an image. The package must be public so the cluster and participants can pull without login.

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

`deploy/loadgen/` runs `loadgen` in `lab-loadgen` with a ServiceAccount bound to a ClusterRole that can only get, list and watch namespaces:

```sh
scripts/deploy-loadgen.sh --version 1.0.N [--dry-run]
```

## Deploy workflow

`.github/workflows/deploy.yml` runs `scripts/deploy.sh` from GitHub Actions, so every deploy is a GitHub deployment in environment `lab` and sends `deployment_status` events:

```sh
gh workflow run deploy.yml -f namespace=<namespace> -f version=1.0.N -f break=none   # or one of the --break types
```

It logs in with the managed identity (OIDC, `azure/login`), gets the AKS kubeconfig and converts it with `kubelogin -l azurecli`, then runs `deploy.sh`. It builds no image. Runs for the same namespace queue (`concurrency: deploy-<namespace>`), they never cancel each other. With `break=none` the job waits for `kubectl rollout status` of `orders`, `inventory` and `payments` (90 s each) and fails if the shop does not become ready. With a breakage it does not wait: unhealthy pods are the expected result, so the job succeeds once `kubectl apply` succeeds. A namespace that `deploy.sh` refuses fails the job.
