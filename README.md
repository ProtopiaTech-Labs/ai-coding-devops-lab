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
