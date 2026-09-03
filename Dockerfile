# syntax=docker/dockerfile:1.7
FROM python:3.12-slim-bookworm AS build
WORKDIR /build
RUN pip install --no-cache-dir build==1.5.0
COPY pyproject.toml README.md ./
COPY src ./src
RUN python -m build --wheel

FROM python:3.12-slim-bookworm AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends nodejs npm ca-certificates \
 && npm install --global @openai/codex@0.145.0 \
 && npm cache clean --force \
 && apt-get purge -y npm \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /build/dist/*.whl /tmp/
RUN pip install --no-cache-dir /tmp/*.whl \
 && rm /tmp/*.whl \
 && useradd --system --uid 10001 --create-home --home-dir /home/windowkeeper windowkeeper \
 && install -d -o windowkeeper -g windowkeeper -m 0700 /data /run/windowkeeper
USER 10001:10001
WORKDIR /home/windowkeeper
ENV WINDOWKEEPER_DATA_DIR=/data \
    WINDOWKEEPER_RUNTIME_DIR=/run/windowkeeper \
    WINDOWKEEPER_HOST=0.0.0.0 \
    WINDOWKEEPER_PORT=8787 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD ["python","-c","import urllib.request; urllib.request.urlopen('http://127.0.0.1:8787/health/live', timeout=2)"]
ENTRYPOINT ["python","-m","codex_broker.container_entrypoint"]
CMD ["serve"]
