# LLM Wiki BFF

Backend-for-frontend API for LLM Wiki. It serves project-scoped wiki sources, concepts, ID routing, import, search/cache/index, and pipeline status endpoints.

## Requirements

- Go 1.26
- Docker for the Compose local integration flow
- `gcloud` and Docker registry access for deploys
- A sibling `llm-wiki-frontend` checkout for the default Compose frontend path

## Test

Run the full Go test suite:

```sh
go test ./...
```

Run the local filesystem store tests only:

```sh
go test ./internal/localfs
```

## Local Development

Local app development uses filesystem-backed storage under `local-data/`. It does not require GCP credentials, GCS, Firestore, Cloud Run, or the worker.

Seed demo data:

```sh
make seed
```

Start BFF only for a fast backend loop:

```sh
make bff-local
```

Start BFF + frontend through Docker Compose:

```sh
make dev
```

BFF listens on `http://localhost:8080`. Frontend listens on `http://localhost:3000`.

Local frontend login is enabled in BFF local mode:

```text
email: demo@llm-wiki.dev
password: demo123456
```

The frontend "Try demo" button uses these credentials.

Local scoped API calls require dev headers:

```text
X-User-ID: local-user
X-Project-ID: demo
```

Example smoke checks:

```sh
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@llm-wiki.dev","password":"demo123456"}'
curl -H 'X-User-ID: local-user' http://localhost:8080/api/v1/projects
curl -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/concepts
curl -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/sources
curl -X POST -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/pipeline/rebuild-index
```

Clean local seeded data:

```sh
make clean-local
```

More detail: [docs/LOCAL_DEV.md](docs/LOCAL_DEV.md).

## Deploy

The final environment mapping and release process are documented in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md). The Makefile uses a full commit SHA image tag and development-only deploy defaults; production promotion is available only through the release-gated GitHub workflow.

Build image:

```sh
make docker-build
```

Push image:

```sh
make docker-push
```

Deploy to Cloud Run:

```sh
make deploy
```

Build, push, and deploy:

```sh
make all
```

Production mode expects GCP credentials and uses GCS/Firestore. Local mode is selected only when `--local` or `LOCAL_DATA_DIR` is set.

### Immutable worker generations

The Cloud Run worker uses the GCS API directly; it does not mount the bucket.
Cloud mode requires `BUCKET`, `USER_ID`, and `PROJECT_ID` and publishes a
create-only generation under `.lwc/publish/generations/`, then commits
`.lwc/publish/current.json` with a GCS generation precondition. The BFF reads
the manifest view when it exists and retains direct legacy reads only for
projects that have not yet published a manifest. `--vault` remains the local
developer workflow and is never used by the deployed worker.

The BFF supports `FIRESTORE_DATABASE_ID`, `PIPELINE_JOB_URL`, and `ALLOWED_ORIGINS` environment overrides. Empty database and pipeline values preserve the legacy defaults; configured pipeline URLs must be HTTPS Cloud Run Jobs `:run` URLs on `run.googleapis.com` with the expected resource path.

## Useful Commands

```sh
make build-sync
go run . --local ./local-data
LOCAL_DATA_DIR=./local-data DEV_JWT=true JWT_SECRET=dev-secret go run . --local ./local-data
```

## Pipeline rate limits (LWC-138)

User `POST /api/v1/pipeline/run` enforces per-project quotas before Cloud Run:

| Env | Default | Meaning |
|-----|---------|---------|
| `PIPELINE_DAILY_LIMIT` | 2 | Max accepted runs per project per UTC day |
| `PIPELINE_COOLDOWN_SECONDS` | 3600 | Min seconds between accepted runs |
| `PIPELINE_MIN_NEW_RAW` | 1 | Require this many new/modified raw files since last run |
| `PIPELINE_DEMO_USER_IDS` | (empty) | Comma-separated user IDs blocked from pipeline |

When Firestore is unavailable, quota is not enforced (`quota.enforced=false`). Admin pipeline trigger skips daily/cooldown/new-raw but still blocks if already running.
