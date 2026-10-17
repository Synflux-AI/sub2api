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

## 结束原因筛选 `finish_reason`

仅用于用量明细与统计卡片，**不**作用于上面的趋势、用户趋势与模型统计接口（它们走预聚合表）：

- `GET /api/v1/admin/usage`、`GET /api/v1/admin/usage/stats`
- `GET /api/v1/usage`、`GET /api/v1/usage/stats`（用户侧，只能查询自己的记录）

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `finish_reason` | string | 逗号分隔的标准化结束原因；`abnormal` 展开为异常结束集合；缺失时不过滤 |

取值与含义见 [USAGE_FINISH_REASON.md](USAGE_FINISH_REASON.md)。非法取值返回 `400 Bad Request`；管理端统计缓存键包含该参数。未采集结束原因的历史行（`NULL`）不会被任何取值命中。
