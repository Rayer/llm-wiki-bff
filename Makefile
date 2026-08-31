REGION ?= asia-east1
IMAGE_REPO ?= asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/llm-wiki-bff
IMAGE_TAG ?= $(shell git rev-parse HEAD)
IMAGE := $(IMAGE_REPO):$(IMAGE_TAG)
FRONTEND_DIR ?= ../llm-wiki-frontend
BFF_PORT ?= 8080
AUTH_PORT ?= 8081
FRONTEND_PORT ?= 3000
LOCAL_LOGIN_EMAIL ?= demo@llm-wiki.dev
LOCAL_LOGIN_PASSWORD ?= demo123456

.PHONY: docker-build docker-push deploy deploy-dev deploy-prod all build-sync setup local-config seed ensure-local-data dev support-bff support-frontend support-pipeline bff-local auth-local frontend-local local-token pipeline-test pipeline-run kill-local clean-local

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy: deploy-dev

deploy-dev:
	gcloud run deploy llm-wiki-bff-dev --project llm-wiki-cloud --image $(IMAGE) --region asia-east1 --platform managed --allow-unauthenticated --service-account lwc-bff-dev@llm-wiki-cloud.iam.gserviceaccount.com --update-secrets "JWT_SECRET=jwt-secret-dev:latest,DEEPSEEK_API_KEY=deepseek-apikey:***" --remove-env-vars "QUERY_EXPANSION_MODEL,QUERY_EXPANSION_REASONING,ANSWER_SYNTHESIS_MODEL,ANSWER_SYNTHESIS_REASONING,QUERY_SELECTION_LIMIT,QUERY_SELECTION_EXPLORATION_SLOTS,QUERY_SELECTION_EVIDENCE_THRESHOLD,QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT,QUERY_EXPANSION_ATTEMPTS,QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY" --update-env-vars "^@^GCP_PROJECT=llm-wiki-cloud@BUCKET=llm-wiki-data-dev@FIRESTORE_DATABASE_ID=llm-wiki-cloud-dev@PIPELINE_JOB_URL=https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline-dev:run@ALLOWED_ORIGINS=https://wiki.dev.rayer.idv.tw,https://llm-wiki-frontend-dev.vercel.app,http://localhost:3000,http://127.0.0.1:3000@AUTH_SERVICE_URL=https://auth.dev.rayer.idv.tw@QUERY_STAGE_CONFIG_PATH=/app/configs/query/dev/query-dev-2026-08-31.1.json@DEV_JWT=false" --port 8080

deploy-prod:
	@echo "Production deploys are release-gated. Run the Promote BFF to Cloud Run (production) GitHub workflow with a verified full commit SHA."
	@exit 1

all: docker-build docker-push deploy-dev

build-sync:
	go build -o lwc-sync ./cmd/sync/

setup: local-config seed
	go mod download
	cd "$(FRONTEND_DIR)" && NODE_ENV=development npm ci --include=dev

local-config:
	@test -d "$(FRONTEND_DIR)" || { echo "Frontend repo not found: $(FRONTEND_DIR)" >&2; exit 1; }
	@printf '%s\n' \
		'NEXT_PUBLIC_API_URL=http://localhost:$(BFF_PORT)' \
		'NEXT_PUBLIC_AUTH_URL=http://localhost:$(AUTH_PORT)' \
		'NEXT_PUBLIC_DEV_USER_ID=local-user' \
		'NEXT_PUBLIC_DEV_PROJECT_ID=demo' \
		> "$(FRONTEND_DIR)/.env.local"

seed:
	rm -rf local-data
	cp -R demo local-data

ensure-local-data:
	@test -d local-data || $(MAKE) seed

dev: local-config ensure-local-data
	@$(MAKE) -j3 bff-local auth-local frontend-local

support-bff: local-config ensure-local-data
	@$(MAKE) -j2 auth-local frontend-local

support-frontend: local-config ensure-local-data
	@$(MAKE) -j2 bff-local auth-local

support-pipeline: local-config ensure-local-data
	@$(MAKE) -j3 bff-local auth-local frontend-local

bff-local:
	PORT=$(BFF_PORT) LOCAL_DATA_DIR=./local-data DEV_JWT=true JWT_SECRET=dev-secret go run ./cmd/bff --local ./local-data

auth-local:
	PORT=$(AUTH_PORT) LOCAL_DATA_DIR=./local-data DEV_JWT=true JWT_SECRET=dev-secret go run ./cmd/auth --local ./local-data

frontend-local:
	cd "$(FRONTEND_DIR)" && NODE_ENV=development npm run dev -- --hostname 127.0.0.1 --port $(FRONTEND_PORT)

local-token:
	@AUTH_URL='http://127.0.0.1:$(AUTH_PORT)' \
		LOCAL_LOGIN_EMAIL='$(LOCAL_LOGIN_EMAIL)' \
		LOCAL_LOGIN_PASSWORD='$(LOCAL_LOGIN_PASSWORD)' \
		python3 -c 'import json, os, urllib.request; payload = json.dumps({"email": os.environ["LOCAL_LOGIN_EMAIL"], "password": os.environ["LOCAL_LOGIN_PASSWORD"]}).encode(); request = urllib.request.Request(os.environ["AUTH_URL"] + "/api/v1/auth/login", data=payload, headers={"Content-Type": "application/json"}); response = json.load(urllib.request.urlopen(request)); token = response.get("access_token"); assert isinstance(token, str) and token, "login response has no access_token"; print(token)'

pipeline-test:
	go test ./cmd/olw_worker

pipeline-run: ensure-local-data
	@test -n "$$LLM_API_KEY" || { echo "LLM_API_KEY is required" >&2; exit 1; }
	go run ./cmd/olw_worker run '[["run","--auto-approve"]]' --vault ./local-data/users/local-user/projects/demo

kill-local:
	@ports='$(BFF_PORT) $(AUTH_PORT) $(FRONTEND_PORT)'; \
	listeners() { for port in $$ports; do lsof -tiTCP:$$port -sTCP:LISTEN 2>/dev/null || true; done | sort -u; }; \
	pids="$$(listeners)"; \
	if [ -z "$$pids" ]; then echo "No local listeners on ports $$ports"; exit 0; fi; \
	echo "Stopping local listeners on ports $$ports (PIDs: $$(echo $$pids))"; \
	kill -TERM $$pids 2>/dev/null || true; \
	remaining="$$pids"; \
	for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do \
		remaining="$$(listeners)"; \
		[ -z "$$remaining" ] && break; \
		sleep 0.1; \
	done; \
	if [ -n "$$remaining" ]; then \
		echo "Force-stopping remaining listeners (PIDs: $$(echo $$remaining))"; \
		kill -KILL $$remaining 2>/dev/null || true; \
	fi

clean-local:
	rm -rf local-data
