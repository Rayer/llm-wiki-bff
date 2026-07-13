GCP_PROJECT ?= llm-wiki-cloud
SERVICE_NAME ?= llm-wiki-bff
REGION ?= asia-east1
IMAGE_REPO ?= asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/llm-wiki-bff
IMAGE_TAG ?= $(shell git rev-parse HEAD)
IMAGE := $(IMAGE_REPO):$(IMAGE_TAG)
BUCKET ?= llm-wiki-data
FIRESTORE_DATABASE_ID ?= llm-wiki-cloud-prod
PIPELINE_JOB_NAME ?= olw-pipeline
PIPELINE_JOB_URL ?= https://run.googleapis.com/v2/projects/$(GCP_PROJECT)/locations/$(REGION)/jobs/$(PIPELINE_JOB_NAME):run
ALLOWED_ORIGINS ?= https://wiki.rayer.idv.tw,https://llm-wiki-frontend.vercel.app

.PHONY: docker-build docker-push deploy all build-sync seed dev bff-local clean-local

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy:
	gcloud run deploy $(SERVICE_NAME) --image $(IMAGE) --region $(REGION) --platform managed --allow-unauthenticated --update-env-vars "^@^GCP_PROJECT=$(GCP_PROJECT)@BUCKET=$(BUCKET)@FIRESTORE_DATABASE_ID=$(FIRESTORE_DATABASE_ID)@PIPELINE_JOB_URL=$(PIPELINE_JOB_URL)@ALLOWED_ORIGINS=$(ALLOWED_ORIGINS)" --port 8080

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
