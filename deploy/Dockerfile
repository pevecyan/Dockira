# docker-manager: Vite UI → Go binary (UI embedded) → Alpine with the docker CLI and the compose
# plugin. Build from the repo root:  docker build -f deploy/Dockerfile -t docker-manager .

FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/docker-manager ./cmd/docker-manager

FROM alpine:3.22
# compose must match the host's major version (v5): pinned, checksum-verified
ARG COMPOSE_VERSION=v5.6.0
ARG COMPOSE_SHA256=40343e21ca777173e69cff5dbafeb37c6f81f3b0d57d9e597f036e95eb63e76a
RUN apk add --no-cache ca-certificates tzdata docker-cli docker-cli-buildx \
 && mkdir -p /usr/local/lib/docker/cli-plugins \
 && wget -qO /usr/local/lib/docker/cli-plugins/docker-compose \
      "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-x86_64" \
 && echo "${COMPOSE_SHA256}  /usr/local/lib/docker/cli-plugins/docker-compose" | sha256sum -c - \
 && chmod +x /usr/local/lib/docker/cli-plugins/docker-compose \
 && docker compose version
COPY --from=go /out/docker-manager /usr/local/bin/docker-manager
# /root/.docker is mounted read-only (registry credentials), but buildx writes its state under
# $DOCKER_CONFIG/buildx on every build: keep that state somewhere writable
ENV BUILDX_CONFIG=/tmp/buildx
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["docker-manager"]
CMD ["serve", "--config", "/etc/docker-manager/config.yaml"]
