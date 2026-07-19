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
    && pip install --no-cache-dir "https://files.pythonhosted.org/packages/5c/5b/0d226bc92e3adeafeb8f0717a6968526bb5e7b4a09a313d888cacc422204/obsidian_llm_wiki-0.8.5-py3-none-any.whl#sha256=a01375b9cc21107e3e1d33b0f3192ad18252406f47bee9ba6ecbd30e122eb337"

RUN git config --system user.email "worker@llm-wiki.local" \
    && git config --system user.name "LWC Worker"

COPY --from=build /out/worker /worker
WORKDIR /tmp
ENTRYPOINT ["/worker"]
