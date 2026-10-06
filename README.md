# T2VA V0.1

T2VA contains a Go Cloud/API service, a separately connected Worker, SQLite task storage, local file storage, and a static browser console.

## Run Cloud

```sh
./start-cloud.sh
```

On first run, the script generates and saves a random `WORKER_TOKEN` in a mode-600 `.env`, sets `COOKIE_SECURE=false` for this plain-HTTP deployment, publishes port 8080 on the server's network interfaces, builds the image, starts Cloud, and checks `/healthz`. It preserves an existing Worker token. Give the token from `.env` to the GPU Worker; do not paste it into chat. Open TCP 8080 in the server/provider firewall, then access `http://SERVER_IP:8080`. SQLite and files persist in the `t2va-data` Docker volume. Startup applies versioned database migrations.

The browser creates an anonymous guest session on first use. Guests can upload images, create tasks, and view only their own history. Guest history belongs to that browser session; clearing its cookies can make the history unrecoverable. Registering or signing in does not transfer guest tasks.

The Cloud sets an HttpOnly, SameSite=Lax session cookie. This deployment script configures `COOKIE_SECURE=false` because the current site uses plain HTTP. On HTTPS deployments, change `.env` to `COOKIE_SECURE=true` before starting Cloud. Browser state-changing requests also require a matching Origin and session CSRF token.

## Deploy Cloud to an Ubuntu server

Copy the project to the server (excluding local `data/` files), then run `./start-cloud.sh` from the project root. The script configures plain-HTTP cookies and publishes port 8080; allow TCP 8080 in the provider/server firewall and open `http://SERVER_IP:8080`. Start the GPU Worker with `CLOUD_URL=http://SERVER_IP:8080` and the matching token from the server's `.env`. The ComfyUI port stays on the GPU computer. The Docker build needs outbound access to download Go modules. SQLite and generated files persist in the named Docker volume `t2va_t2va-data`; back it up before upgrades.

To use HTTPS later, put a TLS reverse proxy in front of Cloud, bind Docker's port to localhost, and set `COOKIE_SECURE=true` in `.env` before starting Cloud.

## Connect a local ComfyUI Worker

The GPU computer runs both ComfyUI and the T2VA Worker. The Worker makes an outbound WebSocket connection to Cloud, downloads task input, calls ComfyUI locally, then uploads the generated video. Cloud never connects directly to the GPU computer. Keep ComfyUI private; do not expose its port to the public internet.

Export your graph from ComfyUI in **API format**. In that JSON, replace the text input value with `{{T2VA_PROMPT}}` and the Load Image input value with `{{T2VA_INPUT_IMAGE}}`. Save it on the Worker computer.

Run the Worker natively on the same OS as ComfyUI when possible:

```sh
INFERENCE_MODE=comfyui \
CLOUD_URL=https://your-cloud-host \
WORKER_TOKEN='the-same-secret-used-by-cloud' \
WORKER_ID=gpu-worker-1 \
COMFYUI_URL=http://127.0.0.1:8188 \
COMFYUI_WORKFLOW=/path/to/workflow_api.json \
go run ./cmd/worker
```

For a local-only test where Cloud is running in WSL on this computer, set `CLOUD_URL=http://127.0.0.1:8080` and use the same `WORKER_TOKEN` configured for Cloud. If Worker runs inside WSL while ComfyUI runs on Windows, `COMFYUI_URL` must be reachable from WSL; with WSL2 NAT, use the Windows host gateway address, while mirrored networking can use localhost.

The Worker defaults to MockEngine when `INFERENCE_MODE` is unset. The Docker Compose Worker file is suitable for MockEngine; a containerized ComfyUI Worker also needs the API workflow file mounted into the container at the path in `COMFYUI_WORKFLOW`.

## Current boundaries

- Browser task, input file, history, and result access is scoped to the server-side guest or user session. Missing and other-owner resources return the same 404 response.
- Existing tasks migrate to the `system/legacy` owner, are hidden from browser principals, and are excluded from Worker scheduling.
- Worker result uploads are stored as unclaimed and become browser-readable only when the matching running task completes successfully.
- The dispatcher permits one active task for its single V0.1 Worker.
- Cloud startup requeues tasks that were left running by a previous Cloud process.
- Worker output uploads are streamed and capped at 500 MiB. Browser image uploads are capped at 20 MiB.
- Public registration has no rate limiting or abuse controls yet. Add those before opening registration broadly.
- `USER_API_KEY` is deprecated and ignored by the session-authenticated Cloud. It does not replace owner checks.
- ComfyUI API-format workflows use the two input placeholders above. The included MiniMax H3 workflow has been exercised in the local GPU/WSL setup; deployment on another GPU host depends on its installed custom nodes, model files, and ComfyUI version.

Use strong Worker secrets, HTTPS/WSS, persistent backups, and a reverse proxy with suitable upload and request time limits in deployment.
