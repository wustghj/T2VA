#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: Docker is not installed or is not on PATH." >&2
  exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
  echo "ERROR: Docker Compose plugin is not available." >&2
  exit 1
fi
if ! command -v openssl >/dev/null 2>&1; then
  echo "ERROR: openssl is required to generate a Worker token." >&2
  exit 1
fi

touch .env
chmod 600 .env

set_env() {
  key="$1"
  value="$2"
  tmp="$(mktemp)"
  if grep -q "^$key=" .env; then
    sed "s|^$key=.*|$key=$value|" .env >"$tmp"
  else
    cat .env >"$tmp"
    printf '%s=%s\n' "$key" "$value" >>"$tmp"
  fi
  cat "$tmp" >.env
  rm -f "$tmp"
}

if ! grep -q '^WORKER_TOKEN=.' .env; then
  set_env WORKER_TOKEN "$(openssl rand -hex 32)"
  echo "Generated WORKER_TOKEN and saved it in .env (mode 600)."
fi

# This deployment uses plain HTTP, so browsers must send the session cookie
# without the Secure attribute. The Worker token remains independently random.
set_env COOKIE_SECURE false
set_env PORT 0.0.0.0:8080

echo "Building and starting T2VA Cloud..."
docker compose -f docker-compose.cloud.yml up -d --build cloud
docker compose -f docker-compose.cloud.yml ps

echo "Checking Cloud health..."
for attempt in $(seq 1 30); do
  if curl --fail --silent http://127.0.0.1:8080/healthz >/dev/null; then
    echo "Cloud is running on port 8080 at the server's network addresses."
    echo "COOKIE_SECURE=false is configured for the current HTTP deployment."
    echo "Keep the Worker token in .env private; do not post it in chat."
    exit 0
  fi
  sleep 2
done

echo "ERROR: Cloud did not become healthy. Recent logs:" >&2
docker compose -f docker-compose.cloud.yml logs --tail=100 cloud >&2
exit 1
