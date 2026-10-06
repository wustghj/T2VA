# T2VA 多用户、游客模式与现代控制台实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在现有 Go/SQLite Cloud 与本地 Worker 上实现多用户/游客身份、任务与文件服务端隔离、可靠单任务调度，以及可访问的现代化控制台。

**Architecture:** 使用版本化 SQLite 迁移增加用户、会话、文件所有权与任务所有权；通过 HttpOnly Cookie 和服务端身份上下文授权浏览器 API，Worker 继续使用独立 Worker Token。按依赖顺序先完成迁移与认证，再实现资源隔离和 Dispatcher 状态约束，最后接入原生静态控制台并做集成验收。

**Tech Stack:** Go 1.24.0、SQLite (`modernc.org/sqlite`)、`golang.org/x/crypto/argon2`、标准库 `net/http`/`crypto/rand`、原生 HTML/CSS/JavaScript、现有 Docker Compose 配置。

**Spec:** `docs/superpowers/specs/2026-10-03-multi-user-guest-console-design.md`

## Global Constraints

- Go 工具链基线为 `go 1.24.0`，使用当前 SQLite 驱动 `modernc.org/sqlite`。
- 不引入大型前端框架；前端继续使用静态 HTML/CSS/JavaScript，除非后续核实发现已有构建链。
- 密码只保存 Argon2id 哈希；会话 token 使用 `crypto/rand` 生成，数据库只保存 token hash。
- 浏览器会话凭证只通过 HttpOnly Cookie 传递，不放进 localStorage/sessionStorage；生产 HTTPS 必须设置 Secure Cookie。
- Worker WebSocket/API 继续由独立 Worker Token 认证，不接受浏览器会话替代。
- 用户与游客主体的任务、历史、输入文件、结果必须在服务端隔离；未授权资源按不存在处理并返回一致的 404。
- 旧任务迁移为普通身份不可访问的 legacy/system owner，不交给首个新用户。
- 不记录密码、令牌或其他凭证；开发日志记录实际完成项、实际运行命令、结果与限制。
- 本地 HTTP 开发必须使用显式开发配置；注册/登录请求体和密码长度必须受限；公网开放注册前需要限速与反滥用保护。
- 每个 Worker 同时最多运行一个任务；完成、失败、断线、迟到消息必须保持调度和持久化状态一致。
- 不提供游客历史迁移、邮箱/手机验证、密码重置、OAuth/SSO、计费、多 Worker 调度或真正的视频推理实现。

## Review Focus

- Cookie 会话携带跨站请求或缺失/不匹配 Origin 时，变更状态的浏览器接口拒绝请求；认证接口按设计允许的入口除外。
- 并发注册规范化后相同的用户名时，数据库唯一约束保证至多一个成功，另一个得到统一冲突响应。
- 过期、撤销、格式错误及随机无效的会话 token 均不能恢复身份，且响应不泄露账号是否存在。
- Worker 上传但未被成功任务引用的结果文件不得被任何浏览器主体下载；元数据写入失败不能留下可访问孤儿资源。
- 分页边界、非法/过大的 limit、未知状态筛选和相同创建时间的多任务均得到有界且稳定的历史结果。

---

## 文件结构与职责

- `internal/task/migrations.go`（新增）：SQLite schema 版本表、可重复/事务化迁移及 legacy 所有权回填。
- `internal/task/task.go`、`internal/task/task_test.go`：Task owner 字段、owner-scoped 创建/查询/分页、迁移与并发转换测试。
- `internal/identity/identity.go`、`internal/identity/identity_test.go`（新增）：主体类型、用户、会话模型和身份 Repository；密码 KDF 与 token 生成/哈希。
- `internal/api/auth.go`、`internal/api/auth_test.go`（新增）：注册、登录、登出、游客初始化、Cookie/CSRF/Origin 校验及身份上下文中间件。
- `internal/api/handler.go`、`internal/api/handler_test.go`：API 路由、认证身份传递、文件/任务授权和兼容策略。
- `internal/api/worker.go`、`internal/api/worker_test.go`：单活动任务调度、断线回队、消息校验及结果文件元数据登记。
- `internal/api/files.go`（新增，如当前 Handler 职责增长明显）：文件元数据 Repository/服务；只在职责清晰时拆分，不做无关重构。
- `cmd/cloud/main.go`：数据库迁移、身份服务、Cookie 安全模式和 API 依赖初始化。
- `web/index.html`：响应式控制台、注册/登录、游客说明、生成表单、状态/结果和分页历史。
- `docker-compose.cloud.yml`、`README.md`：安全配置变量、HTTPS 与部署限制说明。
- `docs/DEVELOPMENT_LOG.md`：逐阶段记录实施范围、验证命令及准确结果。
- `go.mod`、`go.sum`：只加入经核实兼容 Go 1.24 的 Argon2id 依赖（通常为 `golang.org/x/crypto`）。

## 接口契约

- 主体：`identity.Principal{Type identity.PrincipalType, ID string}`；`PrincipalType` 取 `user`、`guest`、`system`。仅 `user`、`guest` 可用于浏览器资源访问。
- 身份服务：`CreateGuest(ctx) (identity.Session, string, error)`、`Register(ctx, username, password) (identity.Session, string, error)`、`Login(ctx, username, password) (identity.Session, string, error)`、`ResolveSession(ctx, rawToken) (identity.Session, error)`、`RevokeSession(ctx, rawToken) error`。第二个返回值为原始 token，仅用于设置 Cookie，不入日志/数据库。
- 任务 Repository：`Create(ctx, principal, task) error`、`GetOwned(ctx, principal, id) (Task, error)`、`ListOwned(ctx, principal, filter) (Page, error)`；旧的无 owner 查询只允许 Worker 调度和内部使用，不可由浏览器 API 调用。
- 浏览器历史：`GET /api/v1/tasks?limit=<1..50>&before=<opaque cursor>&status=<optional>`，按 `created_at DESC, id DESC` 稳定排序，返回 `{tasks,next_cursor}`。
- 认证 API：`POST /api/v1/auth/guest`、`POST /api/v1/auth/register`、`POST /api/v1/auth/login`、`POST /api/v1/auth/logout`、`GET /api/v1/auth/session`。登录/注册创建用户主体会话；游客历史不迁移。
- 文件元数据：每个 ID 记录 owner principal、用途（输入/结果）、创建时间；浏览器下载通过元数据 owner 查询，Worker 输入下载继续使用 Worker Token。

## 任务

### Task 1: 事务化数据库迁移与旧数据兼容

**Files:**
- Create: `internal/task/migrations.go`
- Modify: `internal/task/task.go:NewRepository`
- Test: `internal/task/migrations_test.go`（新增）
- Modify: `cmd/cloud/main.go`

**Interfaces:**
- Produces: `task.Migrate(ctx, db) error`，可安全重复运行；任何迁移失败时整次迁移回滚。
- 版本 1 应建立/确认当前 tasks 表，并建立 schema 版本表；后续版本添加 `owner_type`、`owner_id` 与索引，旧任务统一回填 `system/legacy`。

- [ ] 写迁移测试：空库执行迁移后版本与表齐全，重复执行不改变结果。
- [ ] 写旧版数据库测试：先建立无 owner 的旧 tasks 表并插入记录，迁移后该任务归属为 `system/legacy`。
- [ ] 写失败原子性测试：故意制造迁移冲突，确认版本号和 schema 都未部分前进。
- [ ] 在迁移测试中运行 `go test ./internal/task -run Migration -count=1` 并确认新增用例先失败。
- [ ] 实现基于 schema version 的事务迁移，使用 `PRAGMA table_info`/版本判断避免重复 `ALTER TABLE`；启用外键并为 owner 查询创建索引。
- [ ] 在 `cmd/cloud/main.go` 的 Repository 初始化前调用迁移；将 `NewRepository` 改为校验必要 schema 而不是重复维护另一套建表逻辑。
- [ ] 运行 `go test ./internal/task -run Migration -count=1`，确认迁移用例通过。

### Task 2: 主体与用户/会话存储

**Files:**
- Create: `internal/identity/identity.go`
- Create: `internal/identity/identity_test.go`
- Modify: `go.mod`, `go.sum`
- Modify: `internal/task/migrations.go`

**Interfaces:**
- Produces: `PrincipalType`、`Principal`、`Session`、`Service` 以及本计划“接口契约”所列方法。
- 用户名先按 Unicode NFC 规范化、去除首尾空格并做 Unicode case-fold 后唯一；显示名保留用户提交的规范化拼写。限制 3–32 个 Unicode 字符，只接受字母、数字、下划线和连字符。
- 密码长度按 UTF-8 字节计 12–1024；超限或不足统一返回输入错误。Argon2id 参数初始采用 time=3、memory=64*1024 KiB、threads=2、keyLen=32，并由常量集中声明。
- Session 初始有效期 30 天；原始 token 使用 32 字节 `crypto/rand`，Cookie 名 `t2va_session`，Path `/`，HttpOnly、SameSite=Lax、Secure 由显式 `COOKIE_SECURE` 配置控制。

- [ ] 写测试覆盖用户名规范化与无效字符、最小/最大长度、密码长度边界和 Argon2id 哈希不可反解。
- [ ] 写测试证明数据库只存 token hash，guest/user session 解析主体正确，过期与撤销 session 无法解析。
- [ ] 写唯一用户名并发测试，多个并发注册请求最多一个成功。
- [ ] 运行 `go test ./internal/identity -count=1` 确认测试先因类型/行为缺失失败。
- [ ] 加入 `golang.org/x/crypto` 依赖及提供 NFC/case-fold 所需的标准兼容 Unicode 包依赖；在 `go.mod` 中固定解析出的版本并确认支持 Go 1.24。
- [ ] 实现身份 schema（users、sessions）、Argon2id 编码格式与验证、常数时间 token hash 比较策略、Session CRUD、过期清理入口。
- [ ] 运行 `go test ./internal/identity -count=1` 并确认通过。

### Task 3: 认证 HTTP API、安全 Cookie 与 CSRF

**Files:**
- Create: `internal/api/auth.go`
- Create: `internal/api/auth_test.go`
- Modify: `internal/api/handler.go`
- Modify: `cmd/cloud/main.go`
- Modify: `docker-compose.cloud.yml`

**Interfaces:**
- Consumes: `identity.Service` 与 `identity.Principal`。
- Produces: Handler 身份中间件；`GET /api/v1/auth/session` 回 `{principal:{type,id},username?}`；认证接口返回统一 JSON 错误，不暴露密码哈希/Session/token。
- 缺少 Origin 的浏览器变更请求在同源浏览器 API 中拒绝，需 CSRF token 的方案采用服务端 session 绑定随机 CSRF nonce，并由 session API 返回给页面；所有 POST/PATCH/PUT/DELETE 的浏览器路由验证请求头 `X-CSRF-Token`。Worker 路由不走该机制。

- [ ] 写 httptest 用例覆盖创建游客 Cookie、Cookie 属性、注册、登录、登出、session 查询、错误密码、用户名冲突和请求体过大。
- [ ] 写 CSRF/Origin 用例：同源合法 CSRF token 成功；缺失/错误 token、跨站 Origin 的状态变更请求被拒；Worker Token API 不受影响。
- [ ] 写测试保证密码、hash 与 session token 不出现在响应体或错误文本。
- [ ] 运行 `go test ./internal/api -run 'Auth|CSRF' -count=1` 确认先失败。
- [ ] 注入 Identity Service 与 Cookie secure 配置；实现 auth endpoints、统一错误 JSON、Cookie set/clear 与 session 中间件。
- [ ] 仅在显式 `COOKIE_SECURE=false` 开发配置下允许 HTTP cookie；部署文档注明生产必须 HTTPS 和 Secure Cookie，未设置时默认 Secure。
- [ ] 运行上述 auth/CSRF 测试并确认通过。

### Task 4: 任务主体归属与稳定分页

**Files:**
- Modify: `internal/task/task.go`
- Modify: `internal/task/task_test.go`
- Modify: `internal/task/migrations.go`

**Interfaces:**
- Consumes: `identity.Principal`。
- Produces: owner-scoped 创建/详情/分页 Repository API；浏览器分页 `limit` 限制 1–50，状态只接受 PENDING、RUNNING、SUCCESS、FAILED。

- [ ] 写测试：用户 A 创建任务后用户 B、guest 与 system 对 `GetOwned` 均得到 `ErrNotFound`，A 可读。
- [ ] 写分页测试：固定创建时间的任务以 ID tie-break，跨页无重复/遗漏；状态筛选和 limit 边界正确。
- [ ] 运行 `go test ./internal/task -run 'Owned|Pagination' -count=1` 确认失败。
- [ ] 实现 Task owner 字段、带 owner 条件的 SQL、参数化状态过滤和 `(created_at,id)` 游标分页；禁止把 raw SQL 拼接自用户输入。
- [ ] 运行 `go test ./internal/task -count=1` 并确认通过。

### Task 5: 文件所有权元数据和孤儿隔离

**Files:**
- Create: `internal/api/files.go`
- Create: `internal/api/files_test.go`
- Modify: `internal/task/migrations.go`
- Modify: `internal/api/handler.go`
- Modify: `internal/api/worker.go`

**Interfaces:**
- Produces: 文件元数据 `Register(ctx,id,principal,kind) error`、`GetOwned(ctx,id,principal) (File,error)`、`GetForWorker(ctx,id) (File,error)`。
- 浏览器上传绑定当前主体；Worker 上传输出初始为未关联状态，仅被成功完成任务引用后才可通过任务 owner 下载；Worker 输入下载要求文件为该任务的输入文件。

- [ ] 写测试覆盖上传元数据失败后存储文件被清除、文件不存在/他人所有均统一 404、Worker 上传结果不能直接被浏览器访问。
- [ ] 写测试覆盖 Worker 输出成功绑定任务后仅任务 owner 能读，另一主体及未授权任务输出都不可见。
- [ ] 运行 `go test ./internal/api -run 'FileOwner|Orphan' -count=1` 确认先失败。
- [ ] 增加 files 表与 owner/kind/status 字段及索引；定义 `unclaimed` worker result 状态。
- [ ] 将文件写入与元数据登记封装为补偿流程：登记失败删除磁盘文件；成功任务完成更新结果引用/文件状态，更新失败不得向浏览器暴露。
- [ ] 运行文件测试并确认通过。

### Task 6: Owner-scoped 浏览器任务/文件 API

**Files:**
- Modify: `internal/api/handler.go`
- Modify: `internal/api/handler_test.go`
- Modify: `internal/api/files.go`

**Interfaces:**
- Consumes: 请求上下文中的 `identity.Principal`，不从 JSON、query 或 header 接受客户端指定 owner。
- Produces: `GET /api/v1/tasks` 历史分页、owner-scoped 详情/结果；用户文件访问同样使用 owner 条件；worker routes 继续在身份中间件前单独认证。

- [ ] 写 API 测试验证用户 A 上传/创建/读取资源后，B 与 guest 对文件、任务详情、结果均获得相同 404；A 得到正常响应。
- [ ] 写历史 API 测试验证筛选、稳定游标分页和无效参数 400。
- [ ] 写 API 测试验证客户端提交 `owner_id`/`owner_type` 被拒绝或忽略且无法改变归属。
- [ ] 运行 `go test ./internal/api -run 'Owner|History|Task' -count=1` 确认新增用例失败。
- [ ] 所有浏览器 task/file routes 强制解析 session 主体；创建任务以服务端 principal 写 owner；确认输入文件属当前 principal 后创建。
- [ ] 所有未授权和不存在详情/结果/文件返回同一 404；Worker API 使用独立 Worker Token，不继承浏览器 principal。
- [ ] 将可选 `USER_API_KEY` 改为明确兼容开关，不能绕开 owner 验证；弃用时 README 明确说明。
- [ ] 运行 `go test ./internal/api -count=1` 并确认通过。

### Task 7: Dispatcher 单活动任务状态机

**Files:**
- Modify: `internal/api/worker.go`
- Modify: `internal/api/worker_test.go`
- Modify: `internal/task/task.go`
- Modify: `internal/task/task_test.go`

**Interfaces:**
- Consumes: Repository 的 pending 列表与状态转换。
- Produces: 每 Worker 单活动任务；空闲/无连接时 `current==""`；成功、失败、断线或发送失败后释放当前任务并最多派发一个后续任务。

- [ ] 写回归测试：注册时有多个 pending 任务，仅一个变 RUNNING 并收到一个 `TASK_ASSIGN`。
- [ ] 写并发测试：多个同时 `NotifyTaskCreated` 调用仍只分派一个活动任务，无任务状态丢失。
- [ ] 写生命周期测试：SUCCESS/FAILED 后可继续分派；断线重排队后重连继续；旧连接迟到消息不能完成新任务；WriteJSON 错误清理 current 并重排队。
- [ ] 运行 `go test ./internal/api -run Dispatcher -count=1` 确认失败。
- [ ] 在锁保护下检查 worker connection/current，`assign` 若已有当前任务即拒绝；注册时仅调用一次 `dispatchNext`；消息完成/失败只有匹配 current 且 Worker ID 对应的任务可改变状态。
- [ ] 修复发送失败和清理路径的锁顺序，避免数据库调用/连接切换造成旧连接清理新连接状态；为当前连接设置可验证的 generation/连接身份。
- [ ] 运行 `go test -race ./internal/api ./internal/task -count=1` 与相关测试确认通过。

### Task 8: 控制台信息架构与响应式视觉

**Files:**
- Modify: `web/index.html`

**Interfaces:**
- Produces: 单页原生 HTML/CSS/JS，语义化导航、生成区、历史区、账号区、游客提示和可读状态反馈；API 契约使用本计划上文定义。

- [ ] 编写并运行 HTML 静态检查（若无工具，至少使用浏览器 DOM 验证）确认初始页面 landmark、label、标题层级与按钮语义。
- [ ] 建立桌面/移动响应式布局：桌面侧栏和主工作区，窄屏改为单列；定义可见 focus ring、颜色对比、非颜色状态标识和 reduced-motion 支持。
- [ ] 创建游客状态提示，说明历史绑定当前浏览器且清理数据后可能丢失；提供登录/注册入口及账号面板。
- [ ] 增加生成表单、文件选择预览、任务状态卡片、结果链接、历史空态/加载态/失败态和可键盘操作的通知区域。
- [ ] 用本地浏览器或 Playwright 检查至少 1440px 与 375px viewport；检查键盘 tab、focus、错误提示与控制台无 JS 错误。

### Task 9: 前端认证、生成与历史数据流

**Files:**
- Modify: `web/index.html`
- Modify: `internal/api/handler_test.go`（必要时补 API 契约测试）

**Interfaces:**
- Consumes: `/api/v1/auth/session`、游客创建、login/register/logout、files、tasks create/list/detail/result API。
- CSRF token 只保存在页面内存变量；请求使用 `credentials: 'same-origin'`，不写 localStorage/sessionStorage。

- [ ] 编写浏览器流程测试：首次访问建 guest session、上传图片、建任务、轮询状态、读取历史。
- [ ] 编写登录/注册测试：登录后显示 user 身份，刷新后 session 仍有效；logout 后回到 guest session，不迁移 guest 历史。
- [ ] 编写界面错误测试：上传超限、错误密码、网络断开、任务失败、session 过期均有可访问反馈。
- [ ] 接入初始化 session 和 CSRF 内存态；针对 401/403 清晰恢复；任务轮询停止于终态并避免重复请求泄漏。
- [ ] 将创建任务、历史分页、详情及结果下载接到 owner-scoped API；前端不得通过猜 ID 获取他人资源。
- [ ] 在浏览器测试中验证 375px/1440px、键盘操作、游客流程、账号流程和历史展示。

### Task 10: 配置、文档与开发日志

**Files:**
- Modify: `README.md`
- Modify: `docker-compose.cloud.yml`
- Modify: `docs/DEVELOPMENT_LOG.md`
- Modify: `cmd/cloud/main.go`

- [ ] README 写明注册与游客规则、游客数据不迁移、浏览器清理后游客历史可能丢失、Cookie/HTTPS 配置及公网开放注册前的限速/反滥用要求。
- [ ] Compose 提供明确的 `COOKIE_SECURE` 示例及默认安全值，避免为方便部署将生产 Secure Cookie 默认关闭。
- [ ] 记录每个完成阶段的文件范围、真实执行的验证命令/结果、未执行项及 MockEngine 限制；不得写入秘密。
- [ ] 确认 Docker build context 包含静态 web 文件、数据库挂载路径、环境变量和迁移启动时序一致。

### Task 11: 最终集成、安全边界与部署验收

**Files:**
- Test: `internal/identity/identity_test.go`
- Test: `internal/task/migrations_test.go`, `internal/task/task_test.go`
- Test: `internal/api/auth_test.go`, `internal/api/handler_test.go`, `internal/api/worker_test.go`
- Modify: `docs/DEVELOPMENT_LOG.md`, `README.md`

- [ ] 执行 `go test ./...`；记录每个包结果，失败则先修复并重跑。
- [ ] 执行 `go test -race ./...`；记录通过/失败与运行环境限制。
- [ ] 执行 `go vet ./...`、`go build ./cmd/cloud ./cmd/worker`。
- [ ] 使用占位测试值执行 Cloud/Worker Compose config 校验；执行 Docker build（如果 Docker 可用），分别记录是否执行及结果。
- [ ] 运行可重复的本地 Cloud/Worker 端到端：guest 上传/任务/Worker 完成/结果读取；再证明用户 A 资源对 B 和 guest 不可见，guest 资源对注册用户不可见。
- [ ] 审阅所有浏览器查询是否带 owner 条件、Worker routes 是否保持独立认证、日志是否无凭证、所有错误是否符合 404 不枚举规则。
- [ ] 更新 README 与开发日志，只报告实际验证证据；明确公网部署前尚需注册/登录限速、反滥用、监控与 TLS 运维。

## Spec 覆盖核对

- 多用户免邮箱/手机验证、游客与不迁移：Tasks 2、3、8、9。
- Argon2id、随机 session hash、Cookie、CSRF、HTTPS：Tasks 2、3、10、11。
- 旧数据 legacy owner 与事务迁移：Tasks 1、11。
- 任务/文件/结果所有权、404 不枚举、历史分页：Tasks 4、5、6、9、11。
- Worker Token 分离及 Dispatcher 单活动任务：Tasks 3、5、6、7、11。
- 现代响应式、无障碍与状态反馈：Tasks 8、9。
- 测试、构建、Compose 与运行闭环及过程记录：Tasks 1–11。
- 明确不实现真实视频生成及公网反滥用的上线前置：Tasks 10、11。

## 计划自审

- 已将跨越身份/数据库/API/调度/前端的规格拆成可独立验证的任务，并按接口依赖排序。
- 没有用未定义的 Repository/Service 接口；所有后续消费接口集中列在契约段及对应 Task Interfaces。
- Review Focus 的五类边界条件分别由 Tasks 3、2、5、4、7 的测试覆盖；迁移原子性、Dispatcher 生命周期、游客/用户隔离另由对应任务覆盖。
- 本目录不是 Git repository（此前环境检查确认），因此不执行或声称执行 git commit；计划和阶段记录以工作区文件为准。
- 真实 ComfyUI/MiniMax H3 工作流及公网生产级反滥用不属于本计划实现范围。
