# 开发过程记录

本文件记录关键产品决策、阶段性产物、验证与限制；不记录密码、令牌或其他凭证。

## 2026-10-03 — 版本化任务数据库迁移

### 已完成

- 新增 `internal/task/migrations.go`：以事务和 schema version 管理 tasks 初始表及 owner 字段迁移；旧任务统一标记为 `system/legacy`，并创建 owner/时间索引。
- `NewRepository` 在初始化时执行迁移；Cloud 启动入口也显式迁移，确保 Repository 初始化前 schema 已就绪。
- 新增迁移测试覆盖空库、重复迁移、旧表数据回填与失败回滚。

### 验证

- `go test ./internal/task -run Migration -count=1`：通过。
- `go test ./...`：通过（api/task/worker 包）。
- `go vet ./...`：通过。
- `go build ./cmd/cloud ./cmd/worker`：通过。
- TDD 红灯记录：首次迁移测试运行因 `Migrate` 尚未定义而按预期编译失败；实现后迁移测试通过。

### 限制与后续

- 本目录没有 Git 元数据，未创建 worktree、提交或 Git diff；代码与日志保留在现有工作区。
- 当前仅完成计划 Task 1；用户/游客身份、文件所有权、API 隔离、Dispatcher、控制台及端到端验收仍待实现。

## 2026-10-03 — 多用户、游客模式与控制台改版设计

### 已确认的产品决策

- 目标为多用户注册与登录，不要求邮箱/手机验证。
- 提供游客模式；游客可直接体验核心流程。
- 游客历史依赖当前浏览器匿名会话，清除浏览器数据可能无法找回。
- 游客历史不自动转移到注册账号。
- 用户之间以及用户与游客之间的任务、文件和结果必须在服务端隔离。
- 控制台需要现代化改版，并增加账号入口、任务历史及状态信息。
- 开发过程需留痕；秘密不得进入本日志。

### 本阶段产物

- `docs/superpowers/specs/2026-10-03-multi-user-guest-console-design.md`：架构设计草案，涵盖身份与会话、所有权隔离、前端范围、安全要求、测试验收、分阶段实现及范围外事项。

### 基线检查与限制

- 之前检查确认 Go 测试、`go vet`、Cloud/Worker 构建通过；但没有 Git 元数据，无法可靠审阅历史变更差异。
- 当前任务数据模型尚无 user/guest owner，现有可选全局 `USER_API_KEY` 不是账号隔离机制。
- Worker Dispatcher 单活动任务约束仍需修复并测试。
- 当前 MockEngine 不生成真实可播放视频；真实 ComfyUI/MiniMax H3 工作流仍未完成。
- 本设计尚待用户审阅批准；批准后再创建实施计划和开始产品代码改造。

### 下一步

1. 用户审阅上述设计草案并提出修改或批准。
2. 批准后制定可逐步验证的实施计划，优先认证与所有权边界、然后 UI 与体验。
3. 每个阶段记录代码范围、验证命令及实际结果；没有执行的验证明确标为未执行。

## 2026-10-03 — 用户确认后实现游客/用户隔离后端闭环

### 本阶段实现

- 用户确认多用户/游客设计后开始实施。
- 数据库 schema 升至 v4，增加文件 owner/kind/status/task 元数据；迁移负责建立 users/sessions/files；Repository 构造函数改为校验已迁移 schema。
- identity service 不再自行建表；补上 Argon2id 和 Unicode 依赖声明、未知用户名的等成本密码校验、注册会话失败时清理新用户及过期 session 清理入口。
- Cloud 注入身份服务与文件 Repository；新增 guest/session/register/login/logout API、HttpOnly SameSite Cookie、Secure 默认值、Origin/CSRF 校验及统一认证错误 JSON。
- 浏览器文件、任务创建、详情和历史列表按 session principal 隔离；历史使用稳定 created_at/id 游标，未授权任务/文件返回 404。
- Worker 上传结果登记为 unclaimed；只有匹配的运行中任务成功时，单个事务才把结果文件归属给任务 owner 并完成任务。Worker 输入下载现在需要匹配当前分派任务。
- Dispatcher 改为每个 Worker 只派发一个活动任务，完成/失败后再派下一个；旧连接消息按连接身份拒绝。Worker 输出上传客户端改用流式 multipart。
- README 与 Cloud Compose 增加 Cookie、游客数据、MockEngine、注册反滥用和 USER_API_KEY 兼容说明；Docker 构建关闭 VCS stamping，以支持无 Git 元数据的工作区。

### 当前验证与限制

- 用户确认前只读检查中，`go test ./internal/api ./internal/task ./web -count=1` 与 web 静态测试通过；这些结果是改动前的基线，不代表本阶段代码通过。
- 本阶段之后的 `gofmt` 已运行。
- 本阶段测试/构建尚未完成：当前环境的 Go module cache 是只读的，新加 `golang.org/x/crypto` 与 `golang.org/x/text` 模块无法写入；模块代理网络也受限。使用 `/tmp` 的 Go build cache 后，错误明确指向模块缓存/下载阻断。
- Docker Compose 配置校验在提供占位 `WORKER_TOKEN` 时通过。
- 未运行数据库服务、未启动 Cloud/Worker、未改动 `data/` 下的数据库或运行文件。

### 尚需验证

- 可写 Go module cache 且可访问模块代理的环境中运行 `go test ./...`、`go test -race ./...`、`go vet ./...` 和 Cloud/Worker 构建。
- 端到端检查首次游客会话、登录注册、上传、任务轮询、结果授权和刷新恢复；重点覆盖 Dispatcher 断线/并发边界。
- 公网开放注册前增加限速/反滥用控制。真实 ComfyUI/MiniMax H3 适配器仍未实现。

### 后续可用验证（2026-10-03）

- `GOCACHE=/tmp/t2va-gocache go test ./web -count=1`：通过。
- `GOCACHE=/tmp/t2va-gocache go test ./internal/worker -run 'TestWebsocketURL|TestSafeID|TestMockEngine' -count=1`：通过；未包含需要监听本地端口的上传边界测试。
- `GOCACHE=/tmp/t2va-gocache go build -buildvcs=false ./cmd/worker`：通过。
- `WORKER_TOKEN=validation-placeholder docker compose -f docker-compose.cloud.yml config --quiet`：通过。
- `GOMODCACHE=/tmp/t2va-modcache GOCACHE=/tmp/t2va-gocache go test ./...`、`go vet ./...` 和 Cloud build 均因访问 `proxy.golang.org` 被环境拒绝而未能编译；本阶段新增身份依赖和现有模块 zip 都无法下载。`gofmt -d` 检查通过。
- `GOCACHE=/tmp/t2va-gocache go vet ./internal/worker`：通过。
- 为绕过只读 module cache 做了临时 `/tmp` compile shim（仅提供 x/crypto/x/text 的方法签名，没有替换项目真实依赖）：`go test -modfile=/tmp/t2va-compile.mod -run '^$' ./...` 与 `go vet -modfile=/tmp/t2va-compile.mod ./...` 通过。这只确认整棵源码可编译/静态检查，不验证 Argon2id、Unicode NFC/case-fold 或真实依赖兼容性。
- 同一临时 shim 下，`go test -modfile=/tmp/t2va-compile.mod ./internal/task -count=1` 和 API 的任务创建、游客 Cookie/CSRF、主体隔离/历史用例通过；这些用例未验证密码 KDF 或 Unicode 规范化。Dispatcher WebSocket 生命周期测试因当前沙箱不能监听本地端口，尚未执行。

### 最终复核补充（2026-10-03）

- Worker pending 查询现在排除 `system/legacy`，避免调度缺少文件元数据的迁移前任务；Cloud 启动会将上次进程遗留的 RUNNING 任务重新排队。
- 最终 `gofmt -d` 无差异；临时 compile shim 下全仓库 compile-only 与全仓库 `go vet` 通过，task 全量测试及 API 中任务创建、访客 Cookie/CSRF、文件/任务 owner 隔离与历史用例通过。该 shim 未执行密码或 Unicode 行为，不能替代真实依赖验证。
- 真实依赖可用的验证：web 全量测试、Worker 非网络测试、Worker build 和 Worker vet 通过；Compose config 校验通过。
- 仍未执行真实依赖下的全量测试/Cloud build、Argon2/Unicode 身份测试及需要监听本地端口的 Dispatcher WebSocket 生命周期用例。

## 2026-10-03 — ComfyUI Worker API workflow adapter

- 用户明确要求把本地 ComfyUI/MiniMax 工作流接入 GPU Worker 后，新增 ComfyUIEngine：上传 Cloud 输入图到 ComfyUI、替换 API workflow JSON 中的 prompt/image 标记、提交 `/prompt`、轮询 `/history/{prompt_id}` 并通过 `/view` 下载视频输出。
- `cmd/worker` 的 `INFERENCE_MODE=comfyui` 现在会校验 `COMFYUI_URL`、API 格式 `COMFYUI_WORKFLOW` 和两个占位符，不再启动时直接退出。
- 占位符为 `{{T2VA_PROMPT}}` 和 `{{T2VA_INPUT_IMAGE}}`；Worker 将输入文件按 Cloud 返回的图片 MIME 类型保留扩展名。支持 MP4/WebM/MOV/MKV/AVI/GIF 输出，下载上限 500 MiB，工作流执行总时限默认 45 分钟。
- README 增加本地 Worker 部署、Cloud/ComfyUI 地址说明。实际 MiniMax workflow JSON 尚待用户从 ComfyUI 导出；必须把 prompt 与 Load Image 对应输入替换为上述标记，之后才能做目标机器端到端验证。
- 验证：`go test ./internal/worker -run TestComfyUI -count=1`、`go build -buildvcs=false ./cmd/worker` 及 `go vet ./internal/worker` 通过。ComfyUI HTTP 调用由无网络 mock transport 覆盖；尚未连接用户 GPU 电脑上的真实 ComfyUI。

## 2026-10-06 — 暴露 ComfyUI prompt 校验响应

- 用户在局域网 GPU Worker 实测时报告 ComfyUI `/prompt` 返回 HTTP 400，原任务错误未显示 ComfyUI 响应体。
- Worker 现在会将 ComfyUI 的 400 错误响应（最多 4 KiB）附加到任务错误，以便看到具体节点/输入校验原因。
- `GOCACHE=/tmp/t2va-gocache go test ./internal/worker -run 'TestComfyUI' -count=1`、Worker Windows/Linux 构建及 `go vet ./internal/worker` 通过。
- 需要将更新后的 `internal/worker/comfyui.go` 同步到 GPU 电脑并重启 Worker 后重试，或从 ComfyUI 控制台收集本次失败的节点校验详情；实际工作流根因待该详情确认。

## 2026-10-06 — 移除 API workflow 中未安装的编辑器辅助节点

- GPU 实测错误明确指出节点 `215`（`Fast Groups Bypasser (rgthree)`）在 ComfyUI 服务端不存在。检查确认该节点 `inputs` 为空，且工作流中没有任何节点连接它。
- 从项目的 API workflow 副本 `workflows/minimax_h3_api.json` 移除该孤立的 UI 辅助节点，不改 MiniMax 节点、参数、采样连接或输出。
- JSON 校验通过：20 个节点、prompt/image 占位符各一个、所有节点引用有效、rgthree bypasser 已移除。`go test ./internal/worker -run TestComfyUI -count=1` 和 Worker build 通过。
- 将更新后的 workflow JSON 同步到 GPU 电脑后重试；后续若有其他服务端节点缺失，增强后的任务错误会返回节点名和 ID。

## 2026-10-06 — 修正 MiniMax H3 API 工作流节点枚举

- 用户 GPU 实测发现 ReservedVRAMSetter.auto_max_reserved 仍提交 randomize，且 MiniMaxH3AudioConditioningT8.task_type 提交了 UI 中文标签，均被 ComfyUI API schema 拒绝。该报错说明 GPU 电脑尚未同步上一版 workflow 的 VRAM 数值修正。
- API workflow 已统一修正：auto_max_reserved=0.0；task_type=auto。H3 Conditioning 节点因连有 ref_images.ref_image_0 会自动推断 Ref2VA。MiniMax H3 T8 的上游节点说明将 auto 定义为按已连接输入推断任务类型。
- JSON 与 prompt/image 占位符断言通过；go test ./internal/worker -run TestComfyUI -count=1 通过。尚待复制到 GPU 电脑并在实际 ComfyUI 上再次提交验证。

## 2026-10-06 — 返回 ComfyUI 执行阶段节点错误

- 用户的最新任务错误表明 ComfyUI 已接受 workflow，但执行时状态为 error。Worker 过去没有读取 history status.messages，导致前端只得到通用 execution failed。
- Worker 现解析 execution_error 事件并报告节点 class、ID 和 exception_message；若 ComfyUI 未提供结构化明细，则提示查看 GPU 主机控制台。
- ComfyUI adapter tests、Worker build 与 vet 通过。GPU 电脑需同步 internal/worker/comfyui.go 和 comfyui_test.go 并重启 Worker 后提交新任务，获取实际失败节点。

## 2026-10-06 — 修复移动端历史任务查看按钮

- 确认移动端 760px 断点曾明确隐藏 `.history-item .open-task`，导致手机历史只有提示词/时间和状态。
- 移动端改为三列显示任务摘要、状态与查看按钮，并压缩按钮间距以适配窄屏。
- `GOCACHE=/tmp/t2va-gocache go test ./web -count=1` 通过；需要重新部署 Cloud 静态页面并强制刷新移动浏览器缓存。
