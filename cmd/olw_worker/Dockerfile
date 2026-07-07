FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/worker ./cmd/olw_worker

FROM python:3.12-slim AS worker
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir obsidian-llm-wiki

RUN git config --system user.email "worker@llm-wiki.local" \
    && git config --system user.name "LWC Worker"

COPY --from=build /out/worker /worker
WORKDIR /data
ENTRYPOINT ["/worker"]
