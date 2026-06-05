IMAGE := gcr.io/llm-wiki-cloud/llm-wiki-bff

.PHONY: docker-build docker-push deploy all

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy:
	gcloud run deploy llm-wiki-bff --image $(IMAGE) --region asia-east1 --platform managed --allow-unauthenticated --set-env-vars GCP_PROJECT=llm-wiki-cloud,BUCKET=llm-wiki-data,USER_ID=test-user,PROJECT_ID=demo --port 8080

all: docker-build docker-push deploy
