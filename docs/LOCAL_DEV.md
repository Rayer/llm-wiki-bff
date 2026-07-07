# Local Development

## Quickstart

```sh
make seed
make dev
```

BFF listens on `http://localhost:8080`. Frontend listens on `http://localhost:3000`.

The quickstart uses Docker Compose as the integration path. It mounts `./local-data` into the BFF container and starts BFF with `--local /data`.

## Inner Loop

For faster BFF-only development:

```sh
make seed
make bff-local
```

Local scoped API calls use these headers:

```text
X-User-ID: local-user
X-Project-ID: demo
```

Example:

```sh
curl -H 'X-User-ID: local-user' http://localhost:8080/api/v1/projects
curl -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/concepts
```

## Demo Data

`make seed` copies `demo/` to `local-data/`.

The seeded demo includes prebuilt artifacts:

- `cache/concepts.jsonl`
- `cache/id_map.json`
- `wiki/*.md`
- `wiki/sources/*.md`
- `raw/*.md`
- `wiki.toml`
- `index.md`

The app should load concepts and sources immediately after seeding. Worker and OLW regeneration are outside this local app development flow.

## Frontend Context

Compose builds the frontend from `../llm-wiki-frontend` by default. Override it when the frontend repo is elsewhere:

```sh
FRONTEND_CONTEXT=/path/to/llm-wiki-frontend make dev
```

The compose file exposes these frontend env vars for local header wiring:

```text
NEXT_PUBLIC_DEV_USER_ID=local-user
NEXT_PUBLIC_DEV_PROJECT_ID=demo
```

## Frontend Login

BFF local mode supports a local-only demo account so the frontend login flow can be tested without Firestore:

```text
email: demo@llm-wiki.dev
password: demo123456
```

The frontend "Try demo" button uses the same credentials. After login, the access token identifies `local-user`, so project listing reads `local-data/users/local-user/projects/demo`.

## Troubleshooting

- Port `8080` is busy: run `lsof -i :8080`.
- Port `3000` is busy: run `lsof -i :3000`.
- Demo data is empty: run `make seed`.
- Projects return unauthorized: include `X-User-ID: local-user`.
- Scoped endpoints fail: include `X-Project-ID: demo`.
- Concepts or sources are empty: verify `local-data/users/local-user/projects/demo/cache`.
- Login fails in local mode: use `demo@llm-wiki.dev` / `demo123456`.
- Register returns 503 in local mode: registration still requires the production Firestore-backed path.
