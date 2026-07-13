GCP_PROJECT ?= llm-wiki-cloud
SERVICE_NAME ?= llm-wiki-bff-dev
REGION ?= asia-east1
IMAGE_REPO ?= asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/llm-wiki-bff
IMAGE_TAG ?= $(shell git rev-parse HEAD)
IMAGE := $(IMAGE_REPO):$(IMAGE_TAG)
BUCKET ?= llm-wiki-data-dev
FIRESTORE_DATABASE_ID ?= llm-wiki-cloud-dev
PIPELINE_JOB_NAME ?= olw-pipeline-dev
PIPELINE_JOB_URL ?= https://run.googleapis.com/v2/projects/$(GCP_PROJECT)/locations/$(REGION)/jobs/$(PIPELINE_JOB_NAME):run
ALLOWED_ORIGINS ?= https://llm-wiki-frontend-dev.vercel.app,http://localhost:3000,http://127.0.0.1:3000
RUNTIME_SERVICE_ACCOUNT ?= lwc-bff-dev@llm-wiki-cloud.iam.gserviceaccount.com
JWT_SECRET_NAME ?= jwt-secret-dev

.PHONY: docker-build docker-push deploy deploy-prod all build-sync seed dev bff-local clean-local

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy:
	gcloud run deploy $(SERVICE_NAME) --image $(IMAGE) --region $(REGION) --platform managed --allow-unauthenticated --service-account $(RUNTIME_SERVICE_ACCOUNT) --update-secrets "JWT_SECRET=$(JWT_SECRET_NAME):latest,DEEPSEEK_API_KEY=deepseek-apikey:latest" --update-env-vars "^@^GCP_PROJECT=$(GCP_PROJECT)@BUCKET=$(BUCKET)@FIRESTORE_DATABASE_ID=$(FIRESTORE_DATABASE_ID)@PIPELINE_JOB_URL=$(PIPELINE_JOB_URL)@ALLOWED_ORIGINS=$(ALLOWED_ORIGINS)@DEV_JWT=false" --port 8080

deploy-prod:
	@echo "Production deploys are release-gated. Run the Promote BFF to Cloud Run (production) GitHub workflow with a verified full commit SHA."
	@exit 1

all: docker-build docker-push deploy

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
