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

The Makefile deploy path builds a Docker image, pushes it, and deploys the BFF service to Cloud Run.

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

Current deploy target:

```sh
gcloud run deploy llm-wiki-bff \
  --image gcr.io/llm-wiki-cloud/llm-wiki-bff \
  --region asia-east1 \
  --platform managed \
  --allow-unauthenticated \
  --set-env-vars GCP_PROJECT=llm-wiki-cloud,BUCKET=llm-wiki-data,USER_ID=test-user,PROJECT_ID=demo \
  --port 8080
```

Production mode expects GCP credentials and uses GCS/Firestore. Local mode is selected only when `--local` or `LOCAL_DATA_DIR` is set.

## Useful Commands

```sh
make build-sync
go run . --local ./local-data
LOCAL_DATA_DIR=./local-data DEV_JWT=true JWT_SECRET=dev-secret go run . --local ./local-data
```
