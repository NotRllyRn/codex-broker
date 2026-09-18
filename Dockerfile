# syntax=docker/dockerfile:1.7
FROM golang:1.27-bookworm AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/codex-broker ./cmd/codex-broker

FROM node:22-bookworm-slim AS runtime
RUN apt-get update \
 && apt-get install --yes --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && npm install --global @openai/codex@0.145.0 \
 && npm cache clean --force \
 && rm -rf /usr/local/lib/node_modules/npm /usr/local/bin/npm /usr/local/bin/npx \
 && useradd --system --uid 10001 --create-home --home-dir /home/windowkeeper windowkeeper \
 && install -d -o windowkeeper -g windowkeeper -m 0700 /data /run/windowkeeper
COPY --from=build /out/codex-broker /usr/local/bin/codex-broker
# Existing Compose health commands invoke `python -c` only as a TCP probe.
RUN ln -s /usr/local/bin/codex-broker /usr/local/bin/python \
 && ln -s /usr/local/bin/codex-broker /usr/local/bin/windowkeeper
USER 10001:10001
WORKDIR /home/windowkeeper
ENV WINDOWKEEPER_DATA_DIR=/data \
    WINDOWKEEPER_RUNTIME_DIR=/run/windowkeeper \
    WINDOWKEEPER_HOST=0.0.0.0 \
    WINDOWKEEPER_PORT=8787
EXPOSE 8787 8788
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD ["python","-c","import socket; socket.create_connection(('127.0.0.1', 8787), 2).close()"]
ENTRYPOINT ["codex-broker"]
CMD ["serve"]
