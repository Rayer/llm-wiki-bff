# Local Development

## First-time setup

Keep both repositories under `~/Develop`, then run:

```bash
cd ~/Develop/llm-wiki-bff
make setup
```

## Choose the component you are developing

### BFF

Start everything except the BFF:

```bash
make support-bff
```

This starts the Frontend and prepares seeded pipeline data. Run the BFF separately from your terminal, IDE, or debugger:

```bash
make bff-local
```

### Frontend

Start everything except the Frontend:

```bash
make support-frontend
```

This starts the BFF and prepares seeded pipeline data. Run the Frontend separately from your terminal or debugger:

```bash
make frontend-local
```

### Pipeline

Start everything except the Pipeline:

```bash
make support-pipeline
```

This starts the BFF and Frontend. Run Pipeline tests separately:

```bash
make pipeline-test
```

Run the full Synto pipeline only when provider-backed execution is needed:

```bash
LLM_API_KEY=... make pipeline-run
```

## Run the normal app

If you are not isolating one component:

```bash
make dev
```

## Ports

Defaults:

```text
BFF_PORT=8080
FRONTEND_PORT=3000
```

Override them on any Make target:

```bash
make dev BFF_PORT=18080 FRONTEND_PORT=13000
```

The generated Frontend `.env.local` automatically uses `BFF_PORT`.

## URLs

```text
Frontend: http://127.0.0.1:3000
BFF:      http://127.0.0.1:8080
```

## Local authentication

Local mode provides one admin-capable demo account:

```text
email:    demo@llm-wiki.dev
password: demo123456
user ID:  local-user
role:     admin
```

Get a fresh 15-minute JWT from the running BFF:

```bash
TOKEN="$(make local-token)"
```

Use it for normal or admin APIs:

```bash
curl -fsS \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8080/api/v1/admin/settings
```

When overriding the BFF port:

```bash
TOKEN="$(make local-token BFF_PORT=18080)"
```

The credential and `JWT_SECRET=dev-secret` are local-only. Do not use them for deployed environments, and do not commit a generated JWT.

Press `Ctrl-C` to stop the support processes.

If local processes were orphaned, stop every listener on the configured local ports:

```bash
make kill-local
```

For overridden ports:

```bash
make kill-local BFF_PORT=18080 FRONTEND_PORT=13000
```

## Generated local config

`make setup` creates `~/Develop/llm-wiki-frontend/.env.local`:

```dotenv
NEXT_PUBLIC_API_URL=http://localhost:8080
NEXT_PUBLIC_DEV_USER_ID=local-user
NEXT_PUBLIC_DEV_PROJECT_ID=demo
```

The Makefile supplies the BFF local environment automatically. Reset demo data with:

```bash
make seed
```
