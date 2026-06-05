FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bff .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /bff /bff
COPY static/ /static/
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/bff"]
