# sub2api 项目开发指南

> 本文档记录项目环境配置、常见坑点和注意事项，供 Claude Code 和团队成员参考。

## 一、项目基本信息

| 项目 | 说明 |
|------|------|
| **上游仓库** | Wei-Shaw/sub2api |
| **Fork 仓库** | Synflux-AI/sub2api |
| **技术栈** | Go 后端 (Ent ORM + Gin) + Vue3 前端 (pnpm) |
| **数据库** | PostgreSQL 16 + Redis |
| **包管理** | 后端: go modules, 前端: **pnpm**（不是 npm） |

## 二、本地环境配置

### PostgreSQL 16 (Windows 服务)

| 配置项 | 值 |
|--------|-----|
| 端口 | 5432 |
| psql 路径 | `C:\Program Files\PostgreSQL\16\bin\psql.exe` |
| pg_hba.conf | `C:\Program Files\PostgreSQL\16\data\pg_hba.conf` |
| 数据库凭据 | user=`sub2api`, password=`sub2api`, dbname=`sub2api` |
| 超级用户 | user=`postgres`, password=`postgres` |

### Redis

| 配置项 | 值 |
|--------|-----|
| 端口 | 6379 |
| 密码 | 无 |

### 开发工具

```bash
# golangci-lint（CI 用 v2.13，本地建议装同一版以免版本差异带来的噪音）
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13

# pnpm (前端包管理)
npm install -g pnpm
```

## 三、CI/CD 流水线

### GitHub Actions Workflows

| Workflow | 触发条件 | 检查内容 |
|----------|----------|----------|
| **backend-ci.yml** | pull_request；push 仅 `release`/`main` | 单元测试 + 集成测试 + golangci-lint v2.13 + deploy 脚本测试 |
| **security-scan.yml** | pull_request；push 仅 `release`/`main`；每周一 | govulncheck + pnpm audit |
| **release.yml** | tag `v*` | 构建并发布单架构 Linux amd64 GHCR 镜像（PR 不触发） |

> **功能分支上 push 不再触发 CI。** 需要早期信号请开 draft PR（draft 一样触发
> `pull_request`）。这样做是因为裸 `on: push` 会让同一个 commit 被 push 和
> pull_request 各跑一遍，在同一个 SHA 上留下两份同名 check，分支保护无法据此稳定判定。

### 合并门禁（`release` 分支保护）

合入 `release` 必须通过下面 6 个 GitHub Actions 检查，且已钉死到 GitHub Actions
应用（app id `15368`），其他来源同名的 legacy status 无法冒充：

`test` · `frontend` · `golangci-lint` · `shell` · `backend-security` · `frontend-security`

- **不要给这些 workflow 加 `paths:` 过滤。** workflow 因路径过滤而不触发时，required
  check 永远不会出现，PR 会永久卡在 pending —— 这与"检查失败"不同，无法通过重跑解决。
- `strict`（要求分支与 `release` 同步）保持关闭，否则每次 `release` 有新提交，所有
  在途 PR 都要 rebase 并重跑约 10 分钟 CI。
- `enforce_admins` 必须开启，管理员也不能绕过保护规则。`release` 必须通过 PR 合并，直接
  push 白名单保持为空；当前不额外要求人工审批，6 个 required checks 是合并门禁。已知负载
  敏感的偶发测试见下方。

### 飞书通知

发版只发布 `ghcr.io/synflux-ai/sub2api:<version>` 的 Linux `amd64` 镜像，不发布二进制包、arm64
或 Docker Hub 镜像。`VERSION` 只作为本次构建的临时 artifact 使用，Actions 不会提交或推送任何
版本变更到 `main`；`main` 始终只同步 Wei-Shaw 上游。

`CI` workflow 末尾的 `notify` job 调用 `tools/ci/feishu_notify.py`，把**整体 CI 结果**
推一张飞书卡片（成功绿卡 / 失败红卡 / 有检查未报告则橙卡），成功与失败都会发。

- 六项必需检查分散在 `CI` 与 `Security Scan` 两个 workflow，所以脚本不看当前 workflow
  的结果，而是直接读 head SHA 上的 check-runs 做聚合，凑齐六项才发。任一 workflow
  单独发卡都会在「CI 绿 + Security Scan 红」时误报成绿。
- 没有用 `on: workflow_run`（GitHub 的标准聚合做法）：它**只从默认分支读取 workflow
  文件**，而本仓默认分支是 `main`（上游同步分支）、主线是 `release`，只合进 `release`
  会静默永不触发。
- 失败卡片列出失败的 job 与**具体失败的 step** —— check-run 的 `id` 就是 Actions 的
  job id，`GET /actions/jobs/{id}` 直接返回 `steps[].conclusion`，不必下载日志。
- 需要仓库 secret `FEISHU_WEBHOOK`。缺这个 secret 时脚本静默跳过，所以 fork PR
  （GitHub 不给 fork 传 secret）不会报错，只是没有通知。
- 通知本身永不让 CI 失败：脚本顶层兜底所有异常并 `exit 0`。
- `notify` 会多出一个同名 check-run，**不要**把它加进分支保护的必需检查。
- 纯函数部分有单测：`cd tools/ci && python3 feishu_notify_test.py`，由 `shell` job 执行。

**发版通知**：`release.yml` 的 `notify` job 调用 `tools/ci/feishu_release.py`，在 tag 发版时
推一张卡片（成功 / 失败都发，`needs: [release]` + `if: always()`）。取代了原先那个 Telegram
通知步骤 —— 它在 `TELEGRAM_BOT_TOKEN` 为空时 `exit 0`，而仓库从来没配过这个 secret，
所以从未真正发出过。

卡片内容分两种来源，因为本仓的 tag 有两类：

- **annotated tag**（如 `v0.1.178`）—— `%(contents:body)` 是人工写的发版说明
  （「## 版本亮点 / ## 新增功能 …」），直接用。
- **lightweight tag**（如 `v0.1.177`）—— `%(contents:body)` 只是所指提交的消息体，
  通常是一行 merge commit 标题。**最近 5 个 tag 里有 3 个是这种**，所以脚本会识别出来
  并回退到按 feat/fix/perf 分组的自动 changelog（移植自原 Jenkinsfile 的
  `buildChangelogElements`，两列排版、每组上限 15 条、其余折叠成「+N 项杂项」）。

想让卡片带人工发版说明，就用 annotated tag：`git tag -a v0.1.179 -m "..."`。

版本跨度取 `git describe --tags --abbrev=0 <tag>^`，即最近的**可达** tag。本仓 tag 拓扑
不是线性的（有的发版 tag 打在 sync 线上，例如 `v0.1.177` 并非 `v0.1.178` 的祖先），
取可达 tag 才能让 `git log PREV..TAG` 有意义。

### 已知偶发失败

以下测试断言的是全局分配计数或墙钟超时，会被同机负载影响，遇到时先确认改动无关再重跑，
不要直接改代码（这些文件与上游同源，改动会在每次上游同步时留下分叉）：

- `TestSanitizeOpenAIResponsesToolParameterTypes_RewriteCountIndependentOfHits`（`testing.AllocsPerRun` 读全局 mallocs）
- `TestOpenAIWSHTTPBridgeAcceptsFirstFrameAboveLegacy16MiB`（17MB WS 帧 + 10s 硬编码超时）
- `TestGroupUsageRollupTrigger*`（10s `context.WithTimeout` 覆盖 testcontainers 建库到锁等待）

### CI 要求

- Go 版本必须是 **1.27.0**：三个 workflow 都用 `go-version-file: backend/go.mod` 取版本，随后硬断言 `go version | grep -q 'go1.27.0'`。升级 Go 时要同时改 `backend/go.mod`、`backend-ci.yml`（两处）、`release.yml`、`security-scan.yml` 里的这句断言，**以及三个 Dockerfile 里的 Go 构建镜像**（`Dockerfile` / `deploy/Dockerfile` 的 `ARG GOLANG_IMAGE`、`backend/Dockerfile` 的 `FROM golang:`）。前者漏了 CI 会在版本校验步骤直接失败；**后者漏了 CI 不会报，而是等到有人用这些 Dockerfile 构建时才失败**（`go.mod requires go >= X (running Y; GOTOOLCHAIN=local)`）。
- 前端使用 `pnpm install --frozen-lockfile`，必须提交 `pnpm-lock.yaml`

### 本地测试命令

```bash
# 后端单元测试
cd backend && go test -tags=unit ./...

# 后端集成测试
cd backend && go test -tags=integration ./...

# 代码质量检查
cd backend && golangci-lint run ./...

# 前端依赖安装（必须用 pnpm）
cd frontend && pnpm install
```

## 四、常见坑点 & 解决方案

### 坑 1：pnpm-lock.yaml 必须同步提交

**问题**：`package.json` 新增依赖后，CI 的 `pnpm install --frozen-lockfile` 失败。

**原因**：上游 CI 使用 pnpm，lock 文件不同步会报错。

**解决**：
```bash
cd frontend
pnpm install  # 更新 pnpm-lock.yaml
git add pnpm-lock.yaml
git commit -m "chore: update pnpm-lock.yaml"
```

---

### 坑 2：npm 和 pnpm 的 node_modules 冲突

**问题**：之前用 npm 装过 `node_modules`，pnpm install 报 `EPERM` 错误。

**解决**：
```bash
cd frontend
rm -rf node_modules  # 或 PowerShell: Remove-Item -Recurse -Force node_modules
pnpm install
```

---

### 坑 3：PowerShell 中 bcrypt hash 的 `$` 被转义

**问题**：bcrypt hash 格式如 `$2a$10$xxx...`，PowerShell 把 `$2a` 当变量解析，导致数据丢失。

**解决**：将 SQL 写入文件，用 `psql -f` 执行：
```bash
# 错误示范（PowerShell 会吃掉 $）
psql -c "INSERT INTO users ... VALUES ('$2a$10$...')"

# 正确做法
echo "INSERT INTO users ... VALUES ('\$2a\$10\$...')" > temp.sql
psql -U sub2api -h 127.0.0.1 -d sub2api -f temp.sql
```

---

### 坑 4：psql 不支持中文路径

**问题**：`psql -f "D:\中文路径\file.sql"` 报错找不到文件。

**解决**：复制到纯英文路径再执行：
```bash
cp "D:\中文路径\file.sql" "C:\temp.sql"
psql -f "C:\temp.sql"
```

---

### 坑 5：PostgreSQL 密码重置流程

**场景**：忘记 PostgreSQL 密码。

**步骤**：
1. 修改 `C:\Program Files\PostgreSQL\16\data\pg_hba.conf`
   ```
   # 将 scram-sha-256 改为 trust
   host    all    all    127.0.0.1/32    trust
   ```
2. 重启 PostgreSQL 服务
   ```powershell
   Restart-Service postgresql-x64-16
   ```
3. 无密码登录并重置
   ```bash
   psql -U postgres -h 127.0.0.1
   ALTER USER sub2api WITH PASSWORD 'sub2api';
   ALTER USER postgres WITH PASSWORD 'postgres';
   ```
4. 改回 `scram-sha-256` 并重启

---

### 坑 6：Go interface 新增方法后 test stub 必须补全

**问题**：给 interface 新增方法后，编译报错 `does not implement interface (missing method XXX)`。

**原因**：所有测试文件中实现该 interface 的 stub/mock 都必须补上新方法。

**解决**：
```bash
# 搜索所有实现该 interface 的 struct
cd backend
grep -r "type.*Stub.*struct" internal/
grep -r "type.*Mock.*struct" internal/

# 逐一补全新方法
```

---

### 坑 7：Windows 上 psql 连 localhost 的 IPv6 问题

**问题**：psql 连 `localhost` 先尝试 IPv6 (::1)，可能报错后再回退 IPv4。

**建议**：直接用 `127.0.0.1` 代替 `localhost`。

---

### 坑 8：Windows 没有 make 命令

**问题**：CI 里用 `make test-unit`，本地 Windows 没有 make。

**解决**：直接用 Makefile 里的原始命令：
```bash
# 代替 make test-unit
go test -tags=unit ./...

# 代替 make test-integration
go test -tags=integration ./...
```

---

### 坑 9：Ent Schema 修改后必须重新生成

**问题**：修改 `ent/schema/*.go` 后，代码不生效。

**解决**：
```bash
cd backend
go generate ./ent  # 重新生成 ent 代码（json.RawMessage 字段会生成为同类型的 jsontext.Value，属预期）
git add ent/       # 生成的文件也要提交
```

---

### 坑 10：前端测试看似正常，但后端调用失败（模型映射被批量误改）

**典型现象**：
- 前端按钮点测看起来正常；
- 实际通过 API/客户端调用时返回 `Service temporarily unavailable` 或提示无可用账号；
- 常见于 OpenAI 账号（例如 Codex 模型）在批量修改后突然不可用。

**根因**：
- OpenAI 账号编辑页默认不显式展示映射规则，容易让人误以为“没映射也没关系”；
- 但在**批量修改同时选中不同平台账号**（OpenAI + Antigravity/Gemini）时，模型白名单/映射可能被跨平台策略覆盖；
- 结果是 OpenAI 账号的关键模型映射丢失或被改坏，后端选不到可用账号。

**修复方案（按优先级）**：
1. **快速修复（推荐）**：在批量修改中补回正确的透传映射（例如 `gpt-5.3-codex -> gpt-5.3-codex-spark`）。
2. **彻底重建**：删除并重新添加全部相关账号（最稳但成本高）。

**关键经验**：
- 如果某模型已被软件内置默认映射覆盖，通常不需要额外再加透传；
- 但当上游模型更新快于本仓库默认映射时，**手动批量添加透传映射**是最简单、最低风险的临时兜底方案；
- 批量操作前尽量按平台分组，不要混选不同平台账号。

---

### 坑 11：PR 提交前检查清单

提交 PR 前务必本地验证：

- [ ] `go test -tags=unit ./...` 通过
- [ ] `go test -tags=integration ./...` 通过
- [ ] `golangci-lint run ./...` 无新增问题
- [ ] `pnpm-lock.yaml` 已同步（如果改了 package.json）
- [ ] 所有 test stub 补全新接口方法（如果改了 interface）
- [ ] Ent 生成的代码已提交（如果改了 schema）

## 五、常用命令速查

### 数据库操作

```bash
# 连接数据库
psql -U sub2api -h 127.0.0.1 -d sub2api

# 查看所有用户
psql -U postgres -h 127.0.0.1 -c "\du"

# 查看所有数据库
psql -U postgres -h 127.0.0.1 -c "\l"

# 执行 SQL 文件
psql -U sub2api -h 127.0.0.1 -d sub2api -f migration.sql
```

### Git 操作

```bash
# 同步 Wei-Shaw 上游的 main（upstream 只读）
git fetch upstream
git switch main
git merge --ff-only upstream/main

# 将上游同步合并到本项目 release
git switch release
git merge --no-ff main

# 按需更新本项目远端镜像
git push origin main
git push origin release

# 从 release 创建功能分支
git switch release
git switch -c feat/xxx

# upstream 只允许拉取，不向 Wei-Shaw 反向推送
git remote -v
```

### 前端操作

```bash
# 安装依赖（必须用 pnpm）
cd frontend
pnpm install

# 开发服务器
pnpm dev

# 构建
pnpm build
```

### 后端操作

```bash
# 运行服务器
cd backend
go run ./cmd/server/

# 生成 Ent 代码
go generate ./ent

# 运行测试
go test -tags=unit ./...
go test -tags=integration ./...

# Lint 检查
golangci-lint run ./...
```

## 六、项目结构速览

```
sub2api-bmai/
├── backend/
│   ├── cmd/server/          # 主程序入口
│   ├── ent/                 # Ent ORM 生成代码
│   │   └── schema/          # 数据库 Schema 定义
│   ├── internal/
│   │   ├── handler/         # HTTP 处理器
│   │   ├── service/         # 业务逻辑
│   │   ├── repository/      # 数据访问层
│   │   └── server/          # 服务器配置
│   ├── migrations/          # 数据库迁移脚本
│   └── config.yaml          # 配置文件
├── frontend/
│   ├── src/
│   │   ├── api/             # API 调用
│   │   ├── components/      # Vue 组件
│   │   ├── views/           # 页面视图
│   │   ├── types/           # TypeScript 类型
│   │   └── i18n/            # 国际化
│   ├── package.json         # 依赖配置
│   └── pnpm-lock.yaml       # pnpm 锁文件（必须提交）
└── .claude/
    └── CLAUDE.md            # 本文档
```

## 七、参考资源

- [上游仓库](https://github.com/Wei-Shaw/sub2api)
- [Ent 文档](https://entgo.io/docs/getting-started)
- [Vue3 文档](https://vuejs.org/)
- [pnpm 文档](https://pnpm.io/)
