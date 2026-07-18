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

The dev worker is API-only: deploy it with the environment's `BUCKET` and no
GCSFuse volumes or `/data` mount. Before promotion, verify the job image digest,
its `BUCKET` value, and that both `volumes` and `volumeMounts` are empty.

## GitHub Actions

- `CI` runs vet and tests on `main`, `develop/1.0`, and pull requests targeting either branch. It has no legacy k3s deployment step.
- `Deploy BFF to Cloud Run (dev)` runs from `develop/1.0`, vets and tests the commit, builds one image tagged with the full commit SHA, and deploys that exact image only to `llm-wiki-bff-dev` with the dev bucket, named database, pipeline job, CORS origins, and `DEV_JWT=false`.
- `Promote BFF to Cloud Run (production)` is manually dispatched with a full commit SHA. Configure required reviewers on the `production` GitHub environment to enforce the release gate; the workflow requires that SHA to be an ancestor of `develop/1.0`, verifies a successful exact-SHA dev run, downloads its SHA-named digest artifact, and deploys that digest without rebuilding or resolving an environment tag. After each successful deployment, the workflow adds a unique immutable `:dev-${GITHUB_SHA}` or `:prod-<validated commit SHA>` tag to the exact deployed digest for observability only; these tags are never build or promotion inputs.

The commit-SHA image tag identifies the immutable dev build, and the validated `repository@sha256:...` digest identifies the immutable image promoted to production. The `:dev-${GITHUB_SHA}` and `:prod-<validated commit SHA>` tags are unique immutable deployment records; the digest remains the source of truth.

No credential values belong in workflow files, Makefiles, documentation, or command output. Workload Identity secrets remain GitHub environment secrets.

For a local/manual development deploy, use the Makefile with its immutable commit tag and development-only defaults:

```sh
make docker-build docker-push deploy-dev
```

The Makefile `deploy-dev` target hardcodes the dev service, data resources, runtime service account, Secret Manager references, and `DEV_JWT=false`; command-line environment overrides cannot redirect it to production. `deploy` is only an alias for `deploy-dev`. `make deploy-prod` fails closed; production must use the `Promote BFF to Cloud Run (production)` GitHub workflow with a verified full commit SHA.

## Stuck dev publish lease

Use this break-glass procedure only for a stuck development publish lease. It
does not expire, steal, or automatically take over a lease.

1. Confirm that no Cloud Run execution owned by the development publisher is
   `RUNNING`. Stop and investigate if any such execution exists; do not delete
   its lease.

   ```sh
   gcloud run jobs executions list \
     --job="<dev-pipeline-job>" --region="<region>" --project="<gcp-project>" \
     --filter='status==RUNNING' --format='value(name)'
   ```

   Continue only when the result is empty.

2. Inspect the exact `.lwc/publish/lease.json` object and record its exact object generation. Replace every placeholder with the development values;
   do not put real tenant IDs in a runbook or script.

   ```sh
   BUCKET="<dev-bucket>"
   LEASE_OBJECT="users/<user>/projects/<project>/.lwc/publish/lease.json"
   TOKEN="$(gcloud auth print-access-token)"
   ENCODED_OBJECT="$(printf '%s' "$LEASE_OBJECT" | jq -sRr @uri)"
   curl --fail-with-body \
     -H "Authorization: Bearer ${TOKEN}" \
     "https://storage.googleapis.com/storage/v1/b/${BUCKET}/o/${ENCODED_OBJECT}?fields=name,generation"
   LEASE_GENERATION="<exact-object-generation-from-the-inspection>"
   ```

3. Delete only that exact generation with the GCS JSON API conditional request.
   The `ifGenerationMatch` precondition is mandatory. If the generation
   changed, the request must fail; abort if the generation changed. Never retry
   with a newly observed generation without repeating step 1.

   ```sh
   curl --fail-with-body --request DELETE \
     -H "Authorization: Bearer ${TOKEN}" \
     "https://storage.googleapis.com/storage/v1/b/${BUCKET}/o/${ENCODED_OBJECT}?ifGenerationMatch=${LEASE_GENERATION}"
   ```

   Unconditional deletion can allow concurrent publishers and is prohibited.

4. Verify absence by reading the same object and confirming HTTP 404 before
   rerunning the development publisher. If the object is still present, abort
   and investigate rather than rerunning.

   ```sh
   curl --silent --show-error --output /dev/null --write-out '%{http_code}\n' \
     -H "Authorization: Bearer ${TOKEN}" \
     "https://storage.googleapis.com/storage/v1/b/${BUCKET}/o/${ENCODED_OBJECT}"
   # Expected output: 404
   ```
