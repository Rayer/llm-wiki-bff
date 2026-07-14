REGION ?= asia-east1
IMAGE_REPO ?= asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/llm-wiki-bff
IMAGE_TAG ?= $(shell git rev-parse HEAD)
IMAGE := $(IMAGE_REPO):$(IMAGE_TAG)

.PHONY: docker-build docker-push deploy deploy-dev deploy-prod all build-sync seed dev bff-local clean-local

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy: deploy-dev

deploy-dev:
	gcloud run deploy llm-wiki-bff-dev --project llm-wiki-cloud --image $(IMAGE) --region asia-east1 --platform managed --allow-unauthenticated --service-account lwc-bff-dev@llm-wiki-cloud.iam.gserviceaccount.com --update-secrets "JWT_SECRET=jwt-secret-dev:latest,DEEPSEEK_API_KEY=deepseek-apikey:latest" --update-env-vars "^@^GCP_PROJECT=llm-wiki-cloud@BUCKET=llm-wiki-data-dev@FIRESTORE_DATABASE_ID=llm-wiki-cloud-dev@PIPELINE_JOB_URL=https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline-dev:run@ALLOWED_ORIGINS=https://llm-wiki-frontend-dev.vercel.app,http://localhost:3000,http://127.0.0.1:3000@DEV_JWT=false" --port 8080

deploy-prod:
	@echo "Production deploys are release-gated. Run the Promote BFF to Cloud Run (production) GitHub workflow with a verified full commit SHA."
	@exit 1

all: docker-build docker-push deploy-dev

build-sync:
	go build -o lwc-sync ./cmd/sync/

seed:
	rm -rf local-data
	cp -R demo local-data

dev:
	docker compose up --build

bff-local:
	LOCAL_DATA_DIR=./local-data DEV_JWT=true JWT_SECRET=dev-secret go run . --local ./local-data

clean-local:
	rm -rf local-data
