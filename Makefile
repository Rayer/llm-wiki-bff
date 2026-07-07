IMAGE := gcr.io/llm-wiki-cloud/llm-wiki-bff

.PHONY: docker-build docker-push deploy all build-sync seed dev bff-local clean-local

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy:
	gcloud run deploy llm-wiki-bff --image $(IMAGE) --region asia-east1 --platform managed --allow-unauthenticated --set-env-vars GCP_PROJECT=llm-wiki-cloud,BUCKET=llm-wiki-data,USER_ID=test-user,PROJECT_ID=demo --port 8080

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
