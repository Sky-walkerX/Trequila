# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/monolith ./cmd/monolith

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S trequila && adduser -S -G trequila trequila
WORKDIR /app
COPY --from=build /out/monolith /app/monolith
USER trequila
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget --spider -q http://localhost:8080/ping || exit 1
ENTRYPOINT ["/app/monolith"]
