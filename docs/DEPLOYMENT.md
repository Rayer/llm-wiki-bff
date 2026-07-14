# Deployment

The 1.0 stack is split by environment. The BFF reads the environment-specific values below at startup; an empty `FIRESTORE_DATABASE_ID` keeps the legacy default Firestore database behavior for older local deployments.

| Environment | Cloud Run service | GCS bucket | Firestore database | Pipeline job |
| --- | --- | --- | --- | --- |
| Production | `llm-wiki-bff` | `llm-wiki-data` | `llm-wiki-cloud-prod` | `olw-pipeline` |
| Development | `llm-wiki-bff-dev` | `llm-wiki-data-dev` | `llm-wiki-cloud-dev` | `olw-pipeline-dev` |

The BFF configuration variables are:

| Variable | Purpose |
| --- | --- |
| `GCP_PROJECT` | Google Cloud project containing the BFF resources |
| `BUCKET` | Environment-specific GCS bucket |
| `FIRESTORE_DATABASE_ID` | Named Firestore database; empty selects the default database |
| `PIPELINE_JOB_URL` | HTTPS `run.googleapis.com` Cloud Run Jobs API `:run` URL with the exact project/location/job path |
| `ALLOWED_ORIGINS` | Comma-separated CORS origins; whitespace is trimmed, duplicates removed, and `*` is ignored because credentials are enabled |

## GitHub Actions

- `CI` runs vet and tests on `main`, `develop/1.0`, and pull requests targeting either branch. It has no legacy k3s deployment step.
- `Deploy BFF to Cloud Run (dev)` runs from `develop/1.0`, vets and tests the commit, builds one image tagged with the full commit SHA, and deploys that exact image only to `llm-wiki-bff-dev` with the dev bucket, named database, pipeline job, CORS origins, and `DEV_JWT=false`.
- `Promote BFF to Cloud Run (production)` is manually dispatched with a full commit SHA. Configure required reviewers on the `production` GitHub environment to enforce the release gate; the workflow requires that SHA to be an ancestor of `develop/1.0`, verifies a successful exact-SHA dev run, downloads its SHA-named digest artifact, and deploys that digest without rebuilding or resolving a mutable tag.

No credential values belong in workflow files, Makefiles, documentation, or command output. Workload Identity secrets remain GitHub environment secrets.

For a local/manual development deploy, use the Makefile with its immutable commit tag and development-only defaults:

```sh
make docker-build docker-push deploy-dev
```

The Makefile `deploy-dev` target hardcodes the dev service, data resources, runtime service account, Secret Manager references, and `DEV_JWT=false`; command-line environment overrides cannot redirect it to production. `deploy` is only an alias for `deploy-dev`. `make deploy-prod` fails closed; production must use the `Promote BFF to Cloud Run (production)` GitHub workflow with a verified full commit SHA.
