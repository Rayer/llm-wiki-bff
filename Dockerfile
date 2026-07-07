# Multi-stage Go build
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go generate ./... && CGO_ENABLED=0 go build -ldflags="-s -w" -o /bff .

# Runtime
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /bff /bff

EXPOSE 8080
ENTRYPOINT ["/bff"]
