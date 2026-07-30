# User Balance API

`GET /api/v1/open/balance` lets a customer check their account's wallet balance without going through the panel UI. It is authenticated with a dedicated **user access token** (`sat-…`), not with the `sk-` API key used for inference.

> **This token can only read the balance. It cannot make inference/API requests.** A `sat-…` token and an `sk-…` API key are different credentials and are not interchangeable — using a `sat-…` token against any gateway/inference endpoint will fail authentication, and an `sk-…` API key will not authenticate against this endpoint.

## Obtaining an access token

- Generate the token from your account **profile page**, or ask an administrator to issue one on your behalf.
- Format: `sat-` followed by 64 lowercase hex characters.
- Each account has **at most one** access token at a time.
- **Regenerating** replaces the existing token; the old value stops working immediately. Regenerating also **requires your account password**.
- **Revoking** removes the token entirely and also requires your account password.
- Viewing the current token is not one-time — you can fetch it from your profile page repeatedly.

## Authentication

Send the token in either of these forms:

```text
Authorization: Bearer sat-<64 hex chars>
x-api-key: sat-<64 hex chars>
```

Credentials passed as a query parameter (`?key=`, `?api_key=`, `?access_token=`) are rejected.

## Request

```text
GET /api/v1/open/balance
```

No query parameters or request body.

## Response

On success, the endpoint returns a flat JSON object — **not** the `{code, message, data}` envelope used by the panel API:

```json
{
  "object": "sub2api.balance",
  "schema_version": 1,
  "currency": "USD",
  "balance": 12.3457,
  "frozen_balance": 0.9877,
  "observed_at": "2026-07-31T02:00:00Z"
}
```

| Field | Description |
|-------|-------------|
| `object` | Always `"sub2api.balance"`. |
| `schema_version` | Currently `1`. Bump only on a backward-incompatible field change — treat unknown fields as forward-compatible and check this value if you need to detect a breaking change before it affects you. |
| `currency` | Always `"USD"`. |
| `balance` | Wallet balance, rounded to 4 decimal places. See caveats below. |
| `frozen_balance` | Frozen wallet balance, rounded to 4 decimal places. A reference value — see caveats below. |
| `observed_at` | UTC timestamp (RFC 3339) for when the values were read. |

The response carries `Cache-Control: no-store` — do not cache it.

## Errors

All error responses share the shape `{"code": "...", "message": "..."}`.

| Status | `code` | Meaning |
|--------|--------|---------|
| 401 | `INVALID_ACCESS_TOKEN` | Missing, malformed, or unknown token, **or** the account is deactivated. These cases are deliberately indistinguishable so a caller cannot use the response to probe account state. |
| 429 | `INVALID_AUTH_RATE_LIMITED` | Too many failed authentication attempts from this client; a temporary anti-brute-force lock. |
| 429 | `RATE_LIMITED` | The per-user request rate limit for this endpoint was exceeded (see below). |
| 500 | `BALANCE_UNAVAILABLE` | The balance could not be read at this time. Retry later. |

Both 429 responses carry a `Retry-After` header (seconds). Clients should back off for at least that long before retrying.

## Rate limiting

Requests to this endpoint are limited **per user, per minute**. The limit is administrator-configurable (default 60/min) and is completely separate from your inference RPM limit:

- Calling `/api/v1/open/balance` does not consume any of your inference rate limit or quota.
- Making inference requests does not consume your balance-query rate limit.

## Important: this is your wallet balance, not your subscription quota

`balance` and `frozen_balance` reflect the account's **top-up wallet**, which is billed only for usage that draws down the wallet (e.g. pay-as-you-go). If your account is on a **subscription plan**, usage is drawn from your subscription allowance instead, so this endpoint will typically show `0` — that is expected and does **not** mean your account has no usable quota. Check the panel for your remaining subscription allowance.

## Important: `balance` and `frozen_balance` are not one atomic snapshot

`balance` is served from a cache that decrements in near real time as usage is billed. `frozen_balance` is read separately from the account record. The two values can therefore have slightly different freshness and may disagree by a few seconds under concurrent usage — they are two related numbers observed at approximately the same instant, not a single atomic snapshot. Do not build logic that assumes `balance + frozen_balance` (or any relationship between them) holds exactly at every instant.

## Balance is eventually consistent, not strictly monotonic

This is pre-existing behavior of the underlying balance cache, not something introduced by this endpoint. If the cached balance value expires at the same time usage is being reconciled to the account record, a subsequent read can briefly reflect a slightly older value than the last one you saw — which can make the reported balance appear to increase momentarily even though no funds were added. Treat `balance` as eventually consistent: it converges to the correct value shortly afterward, but do not assert that consecutive reads are strictly non-increasing.

## Example

```bash
curl https://api.example.com/api/v1/open/balance \
  -H 'Authorization: Bearer sat-1f2e3d4c5b6a7980a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6071'
```

```json
{
  "object": "sub2api.balance",
  "schema_version": 1,
  "currency": "USD",
  "balance": 12.3457,
  "frozen_balance": 0.9877,
  "observed_at": "2026-07-31T02:00:00Z"
}
```
