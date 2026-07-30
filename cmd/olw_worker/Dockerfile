FROM golang:1.26-bookworm AS build
WORKDIR /src
ARG BUILD_NONCE
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN : "${BUILD_NONCE:?BUILD_NONCE is required}" \
    && sh ./cmd/olw_worker/validate_build_nonce.sh "${BUILD_NONCE}" \
    && CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.buildNonce=${BUILD_NONCE}" -o /out/worker ./cmd/olw_worker

FROM python:3.12-slim AS worker
ARG BUILD_NONCE
LABEL io.llm-wiki.build.nonce="${BUILD_NONCE}"
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir "https://files.pythonhosted.org/packages/4a/e9/41c6b61338d98820780a43ed075cd77525674c38242110435330771d771b/synto-0.7.0-py3-none-any.whl#sha256=4bc8dcf14b53f45fac32ce737ecf878f1a46d6d0b010c7decbe6c3b7b10afa77"

RUN git config --system user.email "worker@llm-wiki.local" \
    && git config --system user.name "LWC Worker"

COPY --from=build /out/worker /worker
WORKDIR /tmp
ENTRYPOINT ["/worker"]
