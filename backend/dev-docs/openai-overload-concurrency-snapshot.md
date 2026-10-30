# OpenAI overload account concurrency snapshot

## Scope

When an OpenAI upstream attempt fails with `server_is_overloaded` /
"servers are currently overloaded", the gateway records the account's local
concurrency state on that attempt in `ops_error_logs.upstream_errors`, and
writes one log line. The goal is to tell whether overload correlates with how
many requests this gateway is sending to the account.

Detection and capture happen in `appendOpsUpstreamError`
(`internal/service/ops_overload_concurrency.go`), so every OpenAI path that
records an upstream attempt is covered without per-call-site changes. Other
platforms and other OpenAI errors are untouched.

## Event fields

All fields are optional and appear only on OpenAI overload events:

| Field | Meaning |
| --- | --- |
| `account_concurrency` | `accounts.concurrency` of the account when it was selected for this request. |
| `account_load_factor` | `accounts.load_factor` at selection time, when set. |
| `account_inflight` | Redis slot count (`concurrency:account:<id>` + live lease set) at the moment the failure was recorded. **Includes the failing request's own slot.** |
| `account_waiting` | Requests waiting in the account's slot queue at that moment. |

`account_concurrency` / `account_load_factor` are written only when the most
recently selected account matches the failing attempt's `account_id`.
`account_inflight` / `account_waiting` are omitted when Redis cannot be read
within 1s; a value of `0` is always written explicitly.

## Log line

`openai.upstream_overloaded_account_concurrency` (info) with `account_id`,
`kind`, `upstream_status_code`, `upstream_request_id`, and the four fields above.
The log carries no account names or credentials.

## Querying

```sql
SELECT
  (e->>'account_id')::bigint          AS account_id,
  (e->>'account_concurrency')::int    AS cap,
  (e->>'account_inflight')::int       AS inflight,
  (e->>'account_waiting')::int        AS waiting
FROM ops_error_logs, jsonb_array_elements(upstream_errors) e
WHERE e ? 'account_inflight'
  AND created_at > now() - interval '1 day';
```
