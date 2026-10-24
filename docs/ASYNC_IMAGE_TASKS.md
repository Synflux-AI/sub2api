# Asynchronous Image Tasks

Asynchronous image tasks let clients submit long-running OpenAI-compatible image requests without keeping one HTTP connection open. This avoids proxy/CDN response timeouts such as Cloudflare 524 while preserving the existing image routing, billing, moderation, concurrency, and failover behavior.

## Endpoints

The authenticated gateway exposes both `/v1` paths and their existing no-prefix aliases:

```text
POST /v1/images/generations/async
POST /v1/images/edits/async
GET  /v1/images/tasks/{task_id}
```

The aliases are `/images/generations/async`, `/images/edits/async`, and `/images/tasks/{task_id}`.

Only OpenAI and Grok groups are supported. Requests use the same JSON or multipart payload as the corresponding synchronous endpoint. Streaming image requests are rejected because a polled task returns one final JSON result.

## Enabling the feature (object storage)

Asynchronous image tasks are **disabled by default** and gated on object storage. When the switch is off — or the S3 credentials are incomplete — the async endpoints return `404` and never create a task or write to Redis. This is deliberate: without offloading, large `b64_json` results (several MB each, e.g. `gpt-image-1`) would accumulate in Redis and exhaust its memory.

### From the admin UI (recommended)

**Admin → Backup → Async image object storage.** Saving the form takes effect immediately — the object-storage client is rebuilt on the next request, so there is no container restart. With several replicas, the save is broadcast over the same Redis channel as the system settings, so the other replicas drop their cached configuration as well; a failed read of the stored settings is retried after a few seconds instead of keeping the feature off until the next save.

Because the async image storage and the database backup share one S3 client, the form defaults to **reusing the backup S3 configuration**: it borrows the endpoint, region and credentials already configured above and keeps only its own bucket and prefix, so backups stay under `backups/` while images go to `images/`. Leave the bucket empty to use the backup bucket as well. Untick the box to point images at a completely separate account.

Saving requires step-up 2FA when that gate is enabled, for the same reason the backup S3 form does: changing the target redirects generated content to another account.

Turning the switch off stops new submissions but keeps already-accepted tasks pollable, so nothing in flight is stranded.

### From the config file

The admin setting takes precedence. When nothing has ever been saved there, the `image_storage` block in `config.yaml` is used instead, so deployments that enabled the feature before the admin UI existed keep working untouched.

Configure an S3-compatible object store (AWS S3, Cloudflare R2, Aliyun OSS, MinIO, …) in `config.yaml` (all keys also accept the `IMAGE_STORAGE_*` environment overrides):

```yaml
image_storage:
  enabled: true
  endpoint: "https://<account_id>.r2.cloudflarestorage.com"  # AWS 官方可留空
  region: "auto"
  bucket: "my-images"
  access_key_id: "..."
  secret_access_key: "..."
  prefix: "images/"
  force_path_style: false          # MinIO/path-style buckets set true
  public_base_url: ""              # set to return public_base_url/key直链; empty → presigned URL
  presign_expiry_hours: 24         # presigned link TTL when public_base_url is empty
  max_download_bytes: 33554432     # cap when re-hosting an upstream image URL (32MB)
```

When a task completes, each generated image is uploaded to the bucket and the result is rewritten to a compact form: `data[].url` points at the stored object (a permanent `public_base_url/key` link, or a time-limited presigned URL) and `b64_json` is removed. Only this small JSON is stored in Redis. If an upload fails, the task is marked `failed` rather than persisting the raw base64.

To support a different vendor beyond the S3-compatible client, implement the `service.ImageStorage` interface (`Save(ctx, key, contentType, data) (url, error)`) and provide it in place of the S3 implementation.

### Troubleshooting: the endpoints return 404 after enabling

`404 async image tasks are not enabled` means `image_storage` did not resolve to a complete configuration, so the feature stayed off. The route exists either way — the 404 comes from the handler, not from an unregistered path, which makes it easy to mistake for a missing build.

Check the startup log for:

```text
WARN image_storage.enabled is true but object storage is not fully configured; async image tasks are disabled  missing_keys=[...]
```

`missing_keys` names exactly which credentials were empty when the config was loaded.

Note that releases **before v0.1.161 silently dropped `IMAGE_STORAGE_ENDPOINT`, `_BUCKET`, `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY` and `_PUBLIC_BASE_URL`** when they were supplied only through the environment: those keys had no registered default, and viper cannot see an environment variable for a key it does not already know about. Deployments driven purely by `environment:` — which is what `deploy/docker-compose.yml` does by default — therefore reported `enabled: true` with empty credentials and 404'd on every async call. On an affected release the workaround is to also place the `image_storage` block in `/app/data/config.yaml` (copy it from `deploy/config.example.yaml`); once the keys exist in the file, the environment overrides apply normally.

Two further causes of a 404 that are unrelated to storage: the API key's group must be on the **OpenAI or Grok** platform (any other platform, or a key with no group at all, yields `Images API is not supported for this platform`), and a task may only be polled with the **same API key that submitted it** — polling with a different key of the same user returns `image task not found` by design.

## Submit a task

```bash
curl -i https://api.example.com/v1/images/generations/async \
  -H 'Authorization: Bearer sk-...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-image-1",
    "prompt": "A lighthouse during a winter storm",
    "size": "1536x1024"
  }'
```

The server stores the initial task in Redis and responds with `202 Accepted`:

```json
{
  "id": "imgtask_0123456789abcdef",
  "task_id": "imgtask_0123456789abcdef",
  "object": "image.generation.task",
  "status": "processing",
  "created_at": 1784092800,
  "expires_at": 1784179200,
  "poll_url": "/v1/images/tasks/imgtask_0123456789abcdef"
}
```

`Location` contains the polling path and `Retry-After: 3` provides the recommended polling interval.

## Poll a task

Use the same API key that submitted the task:

```bash
curl https://api.example.com/v1/images/tasks/imgtask_0123456789abcdef \
  -H 'Authorization: Bearer sk-...'
```

While work is in progress:

```json
{
  "id": "imgtask_0123456789abcdef",
  "task_id": "imgtask_0123456789abcdef",
  "object": "image.generation.task",
  "status": "processing",
  "created_at": 1784092800,
  "expires_at": 1784179200
}
```

On success, `result` mirrors the synchronous image API body, except each image has been offloaded to object storage: `data[].url` points at the stored object and `b64_json` is stripped (so both URL and base64 upstream formats end up as compact stored links):

```json
{
  "id": "imgtask_0123456789abcdef",
  "task_id": "imgtask_0123456789abcdef",
  "object": "image.generation.task",
  "status": "completed",
  "http_status": 200,
  "image_url": "https://...",
  "result": {
    "created": 1784092923,
    "data": [{"url": "https://..."}]
  },
  "created_at": 1784092800,
  "completed_at": 1784092923,
  "expires_at": 1784179323
}
```

For URL responses, `image_url` mirrors the first `data[].url` for simple clients. On failure, the task reaches `failed` and exposes the original OpenAI-compatible error object where available:

```json
{
  "id": "imgtask_0123456789abcdef",
  "task_id": "imgtask_0123456789abcdef",
  "object": "image.generation.task",
  "status": "failed",
  "http_status": 502,
  "error": {
    "type": "api_error",
    "message": "Upstream request failed"
  },
  "created_at": 1784092800,
  "completed_at": 1784092923,
  "expires_at": 1784179323
}
```

All submit and poll responses include `Cache-Control: no-store`, preventing a CDN from caching the `processing` state. Tasks and results expire 24 hours after their latest state update. A task executes for at most 30 minutes.

Task ownership is scoped to both user and API key. Unknown task IDs and IDs owned by another key both return `404`, avoiding task-existence disclosure. Polling remains available when the completed generation used the key's remaining balance; normal authentication, disabled-key, user, IP, and group checks still apply.

## Hiding upstream image URLs in synchronous responses (`url_mode`)

API-key OpenAI accounts proxy `/v1/images/generations` and `/v1/images/edits` verbatim, so a non-streaming response can carry `data[].url` pointing at the supplier's or relay's domain. `image_storage.url_mode` rewrites those URLs before the response leaves the gateway. It is set on the same admin card (or `image_storage.url_mode` in `config.yaml` while nothing has been saved there), takes effect on save, and is **independent of the async switch**.

| Mode | Behaviour | Needs storage |
|---|---|---|
| `passthrough` (default) | Unchanged; the response is byte-for-byte what the upstream returned. | No |
| `inline` | Download the image and inline it. | No |
| `rehost` | Download the image, upload it with a random UUID key under `prefix`, and return `public_base_url/<prefix><uuid>.<ext>`. | Yes, plus `public_base_url` |

Field shape follows the client's `response_format`:

- `url`: `url` becomes `data:image/...;base64,...` (inline) or our own link (rehost); `b64_json` is removed. This matches the OAuth/Codex direct path. Clients that fetch `url` over HTTP may not accept a `data:` URL, and it is about 1.33× the image size.
- `b64_json`: `b64_json` is written and `url` is removed.
- Not specified: follow the upstream. An upstream `url` stays a `url` field (rewritten as above); an upstream `b64_json` is left alone and a duplicated upstream `url` beside it is dropped. If the account has `images_url_to_b64_json` enabled, the client is treated as preferring `b64_json`, so the existing backfill behaviour is kept. In non-passthrough modes the backfill itself is skipped to avoid downloading twice.

`rehost` deliberately never returns presigned links, because they carry the object-storage endpoint domain. Without `public_base_url` or complete credentials it behaves like `inline`.

### Failure policy

By default the rewrite is best effort:

| Case | Result |
|---|---|
| Download fails (non-2xx, timeout, over `max_download_bytes`, not png/jpeg/webp/gif, private/loopback host, host outside `media_hosts`) | The upstream URL is kept and `openai.images.url_rewrite_failed` is logged. |
| Upload fails | Falls back to a `data:` URL. |
| Per-response inline budget (64 MB) exceeded | Remaining images keep the upstream URL. |

**Strict mode** (`url_rewrite_strict`, the "Return 502 when an image cannot be hidden" checkbox; off by default, hot-toggleable) changes this. If any image in the response would still carry an upstream `http(s)` URL, the whole request returns `502 upstream_error` instead. A failed upload that falls back to a `data:` URL does not count, because nothing leaks. In strict mode:

- The request is **not** retried on another account, because the upstream has already generated (and charged for) the image.
- The request is **not billed**, because the client received no image. The upstream cost is absorbed.
- The account is reported as a failed attempt to the scheduler, the same as any other forward error.
- `openai.images.url_rewrite_strict_rejected` is logged with the number of exposed images.
- If the JSON keepalive has already committed a `200`, the error is written as a JSON error body, the same as other late errors on this endpoint.

Strict mode has no effect when `url_mode` is `passthrough`.

A failure never removes the only image field, so billing (which counts items with a non-empty `url` or `b64_json`) is unaffected; billing data is taken from the upstream body before rewriting. Rewritten responses drop `ETag` and `Content-MD5`.

### Download safety and limits

Downloads go through the account's proxy, reject loopback/private/link-local literals, and check every redirect hop against public addresses. They do **not** use `security.url_allowlist.upstream_hosts` (image CDNs are rarely listed there). Set `security.url_allowlist.media_hosts` to restrict image domains; when the URL allowlist is enabled, only `https` is accepted.

Each download is limited to 60 s and each upload to 60 s, with 120 s for the whole response. Up to 4 images download in parallel per response and 16 per process.

The rewrite runs while the account concurrency slot is still held, so `rehost`/`inline` adds the download (and upload) time to the slot hold time. Watch the images 429 rate (`account_wait_queue_full`, slot acquire timeouts) after enabling it.

Async tasks skip this rewrite, because their results are stored by the task itself when it completes.

### Not covered

These paths can still expose upstream details:

- Grok image generation (`data[].url` from xAI), including composite groups routed to Grok.
- `stream=true` SSE responses, which are forwarded line by line.
- Upstream error messages in error responses, and upstream response headers allowed by the response header filter.

### Operational prerequisites for `rehost`

- Make the image bucket publicly readable, **deny ListBucket**, and add a lifecycle rule (1 day is recommended). Objects are uploaded with `Cache-Control: public, max-age=86400, immutable`.
- Add the new image domain to client-side domain allowlists and any web CSP (`img-src`).
- Other replicas pick up a change through the settings broadcast. If a broadcast is missed, they converge within 30 s.
