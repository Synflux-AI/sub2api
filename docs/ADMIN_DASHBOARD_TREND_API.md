# Admin Usage Aggregate Filter API

管理员趋势接口：

- `GET /api/v1/admin/dashboard/trend`
- `GET /api/v1/admin/dashboard/users-trend`
- `GET /api/v1/admin/dashboard/models`
- `GET /api/v1/admin/usage/stats`

这些接口都支持以下可选精确筛选参数；`/trend?group_by=model` 使用相同语义。

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `stream` | boolean | `true` 仅流式请求，`false` 仅同步请求；缺失时不过滤 |
| `output_tokens` | non-negative integer | 精确匹配输出 Token；`0` 是有效筛选值，缺失时不过滤 |

无效的布尔值、负数或非整数返回 `400 Bad Request`。筛选条件会同时用于趋势聚合和缓存键。
