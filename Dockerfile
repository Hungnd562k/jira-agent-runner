# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runner ./cmd/runner

# Static, shell-less runtime with CA certs (needed for Jira/Telegram HTTPS).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/runner /runner
ENV DB_PATH=/data/tasks.db
VOLUME /data
HEALTHCHECK --interval=1m --timeout=5s --start-period=90s --retries=3 CMD ["/runner", "--env-file=", "--healthcheck"]
# Env comes from the container environment (compose env_file), not a file inside the image.
ENTRYPOINT ["/runner", "--env-file="]
