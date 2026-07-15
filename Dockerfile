# Multi-stage Go build
FROM golang:1.26-alpine AS build

ARG APP_VERSION=dev
ARG GIT_SHA=unknown
ARG GIT_BRANCH=unknown
ARG GIT_TAG=

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go generate ./... && CGO_ENABLED=0 go build -ldflags="-s -w \
  -X github.com/rayer/llm-wiki-bff/internal/buildinfo.ProductVersion=${APP_VERSION} \
  -X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitSHA=${GIT_SHA} \
  -X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitBranch=${GIT_BRANCH} \
  -X github.com/rayer/llm-wiki-bff/internal/buildinfo.GitTag=${GIT_TAG} \
  -X github.com/rayer/llm-wiki-bff/internal/buildinfo.ImageTag=${GIT_SHA}" -o /bff .

# Runtime
FROM gcr.io/distroless/static-debian12:nonroot

ARG APP_VERSION=dev
ARG GIT_SHA=unknown
ARG GIT_BRANCH=unknown
ARG GIT_TAG=

LABEL org.opencontainers.image.version=${APP_VERSION} \
      org.opencontainers.image.revision=${GIT_SHA} \
      org.opencontainers.image.ref.name=${GIT_BRANCH} \
      io.llm-wiki.git.branch=${GIT_BRANCH} \
      io.llm-wiki.git.tag=${GIT_TAG} \
      io.llm-wiki.image.tag=${GIT_SHA}

COPY --from=build /bff /bff

EXPOSE 8080
ENTRYPOINT ["/bff"]
