# Codex Proxy 项目解析与 Rust 重写指南

本文档面向准备将当前 Go 版 `codex-proxy` 重写为 Rust 版本的开发者，目标不是逐行翻译，而是帮助你快速理解这个项目的职责边界、运行链路、关键状态机，以及 Rust 中更合适的实现方式。

## 1. 这个项目到底在做什么

这是一个运行在本地或服务器上的 HTTP 代理服务，核心职责是：

1. 对外暴露 OpenAI/Claude 兼容接口。
2. 对内把请求转换成 Codex/Responses API 可接受的格式。
3. 从本地账号池中选择一个可用账号，为请求注入 `access_token` 和 `Chatgpt-Account-Id`。
4. 在上游失败时自动切换账号重试，而不是立刻把错误暴露给客户端。
5. 在后台持续刷新 token、做健康检查、查询额度，并根据结果调整账号状态。

所以它本质上不是“简单反向代理”，而是一个带协议转换、账号调度、状态管理、后台任务和流式转发的网关。

## 2. 项目结构总览

当前 Go 项目可以按 6 个层次理解：

### 2.1 入口层

- `main.go`
  - 读取配置。
  - 初始化 `Manager`、`Executor`、HTTP 路由。
  - 启动后台任务：保存 worker、token 刷新、健康检查、连接保活。
  - 启动 Gin HTTP 服务并做优雅关闭。

### 2.2 配置层

- `internal/config/config.go`
  - YAML 配置反序列化。
  - 默认值填充。
  - 日志级别清洗。

### 2.3 认证与账号管理层

- `internal/auth/types.go`
  - 定义账号、token、额度、统计信息。
  - 维护账号状态：`active` / `cooldown` / `disabled`。
  - 维护原子热路径字段，避免高并发时频繁加锁。
- `internal/auth/manager.go`
  - 加载账号文件。
  - 热加载新增账号。
  - 并发刷新 token。
  - 401 后异步刷新。
  - 异步写回磁盘。
  - 手动刷新和 SSE 进度回传。
- `internal/auth/refresh.go`
  - 通过 `refresh_token` 调 OpenAI OAuth 刷新 token。
- `internal/auth/selector.go`
  - 可用账号过滤。
  - 轮询选择。
  - 按额度使用率排序。
- `internal/auth/health.go`
  - 后台健康检查。
- `internal/auth/quota.go`
  - 查询 `wham/usage` 并缓存额度信息。

### 2.4 协议转换层

- `internal/translator/request.go`
  - OpenAI/Responses 请求转 Codex 请求。
  - 工具 schema 修复。
  - 工具名缩短与还原映射。
- `internal/translator/response.go`
  - Codex SSE 转 OpenAI Chat Completions。
- `internal/translator/claude.go`
  - Claude Messages <-> OpenAI/Codex 转换。

### 2.5 思考参数层

- `internal/thinking/*.go`
  - 解析模型名后缀，例如 `gpt-5.4-high-fast`。
  - 写入 `reasoning.effort` 和 `service_tier`。

### 2.6 执行与 HTTP 层

- `internal/executor/codex.go`
  - 拼上游请求。
  - 负责“选择账号 -> 发请求 -> 失败换号 -> 成功后才开始向客户端写响应”。
  - 处理流式、非流式、Responses、Compact、Claude 原始流。
- `internal/handler/proxy.go`
  - OpenAI 兼容 HTTP 路由。
- `internal/handler/claude.go`
  - Claude Messages 路由。

## 3. 最重要的运行链路

### 3.1 普通 Chat Completions 请求链路

1. 客户端请求 `POST /v1/chat/completions`。
2. `ProxyHandler.handleChatCompletions` 读取 body，拿到 `model` 和 `stream`。
3. `Executor.ExecuteStream` 或 `Executor.ExecuteNonStream` 被调用。
4. `thinking.ApplyThinking` 解析模型后缀并修改请求体。
5. `translator.ConvertOpenAIRequestToCodex` 将 OpenAI 格式改成 Codex/Responses 格式。
6. `sendWithRetry` 循环执行：
   - 从 `Manager.PickExcluding` 选账号。
   - 注入认证头。
   - 发往上游 `/responses`。
   - 如果 2xx，返回成功响应。
   - 如果 401/429/5xx，记录账号失败并换号重试。
7. 成功后才开始向客户端写 HTTP 头和 SSE/JSON。
8. 响应结束后记录 usage、请求成功次数。

这个“成功后才向客户端写 header”非常关键，它保证了流式接口即使内部换号重试，客户端也感知不到中途失败。

### 3.2 Token 后台刷新链路

1. `main.go` 启动 `Manager.StartRefreshLoop`。
2. 循环里有两个 ticker：
   - 扫描账号目录的新文件。
   - 按配置刷新到期 token。
3. `filterNeedRefresh` 会跳过：
   - 还没快过期的 token。
   - 刚刷新过的账号。
   - 已经正在刷新的账号。
4. `refreshAccount` 并发刷新单个账号。
5. 刷新成功后更新内存状态，并异步写回磁盘。
6. 刷新失败时：
   - 429 -> 冷却，不删账号。
   - 其他失败 -> 从内存和磁盘删除账号。

### 3.3 401 异步恢复链路

当正常请求命中 401：

1. 当前请求失败账号被临时标记为 cooldown。
2. 由 `HandleAuth401` 在后台异步刷新 token。
3. 刷新成功恢复账号。
4. 刷新失败则删号。

这条链路的目的，是避免每次 401 都同步阻塞业务请求。

### 3.4 健康检查与额度链路

- 健康检查：向 `/responses` 发一个最小探测请求。
- 额度查询：请求 `https://chatgpt.com/backend-api/wham/usage`。
- 两者都会影响选号：
  - `cooldown` 账号直接跳过。
  - 配额耗尽账号会被视为高使用率。
  - 已查询过额度的账号会按 `used_percent` 升序排序，优先选“最空闲”的号。

## 4. 当前项目的核心设计点

### 4.1 账号状态机

每个账号都可以视为一个有限状态机：

- `active`: 可直接被选中。
- `cooldown`: 暂时不可用，到时间自动重新可用。
- `disabled`: 永久不可用。

状态切换来源：

- 刷新限频 -> `cooldown`
- 请求 429 -> `cooldown`
- 请求 403 -> `cooldown`
- 刷新失败 / 严重认证错误 -> 删除账号或 `disabled`

Rust 版建议显式建模成：

```rust
enum AccountStatus {
    Active,
    Cooldown { until: std::time::Instant },
    Disabled { reason: DisableReason },
}
```

如果需要持久化时间点，再单独保留 `chrono::DateTime<Utc>` 或 Unix 时间戳，不要把“内存计时”和“磁盘时间”混成一个字段。

### 4.2 热路径无锁优化

Go 版本在账号选择路径上做了很多优化：

- `accountsPtr` 保存只读快照。
- `atomicStatus` / `atomicCooldownMs` / `atomicUsedPct` 用于快速判断可用性。
- selector 维护可用账号缓存，TTL 1 秒。

这说明作者预期账号量较大，甚至到万级。

Rust 重写时，建议：

- 全局账号表使用 `Arc<ArcSwap<Vec<Arc<Account>>>>` 或 `ArcSwap<Vec<Arc<Account>>>`。
- 单账号热字段用 `AtomicU8` / `AtomicI64` / `AtomicBool`。
- 冷路径详细数据使用 `RwLock<AccountInner>`。

一个可行结构：

```rust
struct Account {
    file_path: PathBuf,
    status: AtomicU8,
    cooldown_until_ms: AtomicI64,
    used_pct_x100: AtomicI64,
    refreshing: AtomicBool,
    inner: parking_lot::RwLock<AccountInner>,
}
```

### 4.3 “协议转换”和“请求执行”分离

当前设计把“如何把请求转成 Codex”与“如何拿账号发出去”拆开了，这是正确的。Rust 版也建议继续拆层：

- `translator`: 纯 JSON 转换，不发网络请求。
- `executor`: 只管上游交互、认证头、错误处理、重试。
- `handler`: 只做 HTTP 协议适配。

这样 Claude/OpenAI/Responses 三种入口都能复用同一个执行器。

### 4.4 上游始终返回 SSE

这个项目里一个容易忽略的点是：即使“非流式”接口，Codex 上游也可能仍以 SSE 事件返回，代码再从 `response.completed` 中提取最终结果。

Rust 版不要假设“非流式就一定是普通 JSON body”。要把上游响应统一当作事件流处理，至少保留这两种能力：

- 原样透传 SSE。
- 扫描 SSE，抽取 `response.completed` 聚合成最终 JSON。

## 5. Rust 重写时建议的 crate 划分

如果你想做成可维护版本，建议从一开始就按 crate/module 分层，而不是全部堆在 `main.rs`。

### 5.1 推荐目录

```text
src/
├── main.rs
├── app.rs
├── config.rs
├── http/
│   ├── routes.rs
│   ├── middleware.rs
│   └── error.rs
├── auth/
│   ├── mod.rs
│   ├── account.rs
│   ├── manager.rs
│   ├── refresh.rs
│   ├── selector.rs
│   ├── health.rs
│   └── quota.rs
├── executor/
│   ├── mod.rs
│   └── codex.rs
├── translator/
│   ├── mod.rs
│   ├── openai.rs
│   ├── responses.rs
│   └── claude.rs
├── thinking/
│   ├── mod.rs
│   ├── suffix.rs
│   └── apply.rs
└── model/
    ├── config.rs
    ├── account.rs
    └── api.rs
```

### 5.2 推荐依赖

- Web 服务：`axum`
- HTTP 客户端：`reqwest`
- 异步运行时：`tokio`
- 配置：`serde`, `serde_yaml`
- JSON：`serde_json`
- 日志：`tracing`, `tracing-subscriber`
- 时间：`chrono`
- 错误：`thiserror`, `anyhow`
- 原子快照：`arc-swap`
- 锁：`parking_lot`
- SSE：可直接用 `axum::response::sse`，或手动写 streaming body
- UUID：`uuid`
- JWT payload 解码：`base64`

如果你希望更接近 Go 版的高性能 JSON patch 风格，也可以评估 `simd-json`，但第一版不建议过早优化。

## 6. Go 模块到 Rust 模块的映射建议

| Go 文件 | Rust 建议位置 | 说明 |
|---|---|---|
| `main.go` | `main.rs` + `app.rs` | 启动与依赖装配 |
| `internal/config/config.go` | `config.rs` | 配置加载与默认值 |
| `internal/auth/types.go` | `auth/account.rs` | 账号状态与统计 |
| `internal/auth/manager.go` | `auth/manager.rs` | 账号生命周期核心 |
| `internal/auth/refresh.go` | `auth/refresh.rs` | OAuth 刷新逻辑 |
| `internal/auth/selector.go` | `auth/selector.rs` | 选号策略 |
| `internal/auth/health.go` | `auth/health.rs` | 健康检查任务 |
| `internal/auth/quota.go` | `auth/quota.rs` | 额度查询任务 |
| `internal/thinking/*` | `thinking/*` | 模型后缀与思考参数 |
| `internal/translator/request.go` | `translator/openai.rs` | 请求转换 |
| `internal/translator/response.go` | `translator/responses.rs` | SSE -> OpenAI |
| `internal/translator/claude.go` | `translator/claude.rs` | Claude 兼容 |
| `internal/executor/codex.go` | `executor/codex.rs` | 上游执行器 |
| `internal/handler/*.go` | `http/routes.rs` | 路由与 handler |

## 7. Rust 版的关键数据结构建议

### 7.1 配置结构

```rust
#[derive(Debug, Clone, serde::Deserialize)]
pub struct Config {
    pub listen: String,
    pub auth_dir: String,
    pub proxy_url: Option<String>,
    pub base_url: String,
    pub log_level: String,
    pub refresh_interval: u64,
    pub max_retry: usize,
    pub health_check_interval: u64,
    pub health_check_max_failures: usize,
    pub health_check_concurrency: usize,
    pub refresh_concurrency: usize,
    pub api_keys: Vec<String>,
}
```

建议给它实现 `Default` 和 `sanitize()`，逻辑和 Go 版一致。

### 7.2 账号结构

不要把所有字段都塞进一个大锁里。建议拆成：

- 高频只读/写：原子字段。
- 低频元数据：`RwLock<AccountInner>`。

### 7.3 Manager 结构

`Manager` 应至少负责：

- 持有账号快照。
- 文件路径到账号的索引。
- 刷新器和选择器。
- 后台任务入口。
- 保存队列。

Rust 草图：

```rust
pub struct Manager {
    accounts: arc_swap::ArcSwap<Vec<Arc<Account>>>,
    index: parking_lot::RwLock<HashMap<PathBuf, Arc<Account>>>,
    refresher: Refresher,
    selector: Arc<dyn Selector + Send + Sync>,
    auth_dir: PathBuf,
    refresh_interval: Duration,
    refresh_concurrency: usize,
    save_tx: tokio::sync::mpsc::Sender<Arc<Account>>,
}
```

## 8. 请求执行器怎么在 Rust 里落地

`Executor` 是最值得保持原设计的一层。

### 8.1 必须保留的行为

- 内部重试在写客户端响应之前完成。
- 每次重试都要换账号。
- 401 要触发后台 refresh。
- 429/403 要更新账号状态。
- 流式和非流式共用一套重试框架。

### 8.2 推荐实现方式

写一个统一的内部函数：

```rust
async fn send_with_retry(
    &self,
    request_body: Bytes,
    model: &str,
    stream: bool,
    retry: RetryConfig,
) -> Result<(reqwest::Response, Arc<Account>), UpstreamError>
```

然后：

- OpenAI chat stream -> 扫描上游 SSE 并做 chunk 转换。
- OpenAI chat non-stream -> 扫描到 `response.completed` 再拼 JSON。
- Responses stream -> 原样透传字节流。
- Responses non-stream -> 从 SSE 提取 `response`。
- Claude -> 复用原始 Codex SSE，再转 Claude 事件。

### 8.3 连接复用

Go 版显式调了 `Transport` 参数和 keep-alive ping。Rust 版也应该保留：

- 全局复用一个 `reqwest::Client`。
- 配置连接池大小、超时。
- 后台定时发 `HEAD` 或轻量请求保活。

## 9. 协议转换在 Rust 里怎么写更稳

Go 版大量使用 `gjson/sjson` 对原始 JSON 做局部 patch，优点是快，缺点是代码可读性一般。

Rust 版有两条路：

### 路线 A：`serde_json::Value` 为主

优点：

- 可读性更好。
- 更容易写单元测试。
- 更适合维护。

缺点：

- 分配更多。

### 路线 B：typed struct + `Value` 混合

建议实际采用：

- 外层核心字段用 struct。
- 不稳定字段、透传字段用 `Value`。

例如：

- OpenAI request 可定义一小部分 typed struct。
- tools schema、response_format 等复杂嵌套保留 `Value`。

### 需要特别保留的转换行为

1. `system` -> `developer`
2. `messages` -> `input`
3. `tool` 消息 -> `function_call_output`
4. assistant `tool_calls` -> 独立 `function_call`
5. `response_format` -> `text.format`
6. `reasoning_effort` / `variant` -> `reasoning.effort`
7. 工具名长度超过 64 时缩短，并在响应时逆向还原
8. 所有 `type=array` 且缺少 `items` 的 schema 自动补 `items: {}`

第 7 和第 8 点非常容易在重写时漏掉，但它们是兼容性细节。

## 10. 流式处理在 Rust 里要注意什么

这是重写最容易踩坑的部分。

### 10.1 不要一次性把 SSE 全读完

对于流式接口，应使用 `bytes_stream()` 或 `StreamExt` 持续读取，边读边解析。

### 10.2 你需要一个“事件增量解析器”

Codex 返回的是 SSE 行流，因此建议写一个通用组件：

- 输入：`impl Stream<Item = Result<Bytes, reqwest::Error>>`
- 输出：完整的 SSE event line 或 `data:` payload

再在上层做：

- OpenAI chunk 转换器
- Claude event 转换器
- 非流式 `response.completed` 提取器

### 10.3 先拿到 2xx 再返回 streaming response

在 `axum` 里，streaming body 一旦开始返回，header 就已经出去了。因此一定要先完成：

- 账号选择
- 上游请求
- 重试

之后再把成功响应包装成 stream 返回给客户端。

## 11. 后台任务在 Rust 里的组织方式

Go 用 goroutine + context；Rust 版建议用 `tokio::spawn` + `CancellationToken` 或 `Notify`。

至少有这些任务：

- token 定时刷新
- auth 目录热加载扫描
- save queue worker
- 健康检查
- 连接保活

建议用一个 `AppState` 持有这些服务，并在 shutdown 时统一取消。

## 12. 最值得先写测试的模块

Rust 版很适合用测试把协议兼容层锁死，推荐优先写这些：

1. `thinking::suffix`
   - `gpt-5.4-high-fast`
   - `gpt-5.1-codex-max`
   - 数字预算后缀
2. `translator::openai`
   - messages -> input
   - system -> developer
   - tool_calls 转换
   - array schema 修复
3. `translator::responses`
   - `response.output_text.delta` -> OpenAI chunk
   - `response.completed` -> usage / finish_reason
4. `translator::claude`
   - Claude request -> OpenAI
   - Codex SSE -> Claude SSE
5. `auth::selector`
   - cooldown 过滤
   - used_percent 排序
   - round robin 行为

然后再补集成测试：

- 模拟 401 后重试另一个账号
- 模拟 429 后账号冷却
- 模拟非流式接口但上游返回 SSE

## 13. 推荐的 Rust 重写顺序

不要一口气全部重写，建议按下面顺序推进：

### 阶段 1：跑通最小代理

- 配置加载
- 账号加载
- `/health`
- `/v1/chat/completions` 非流式
- 单账号直连上游

### 阶段 2：补齐核心兼容能力

- thinking 后缀
- OpenAI request/response 转换
- 流式 Chat Completions
- Responses API

### 阶段 3：补上生产能力

- 多账号选择
- 内部重试
- 401 后后台刷新
- token 定时刷新
- save worker

### 阶段 4：补高级兼容与运维能力

- Claude Messages API
- `/stats`
- `/refresh`
- `/check-quota`
- 健康检查
- 连接保活

### 阶段 5：性能优化

- 原子快照
- 可用账号缓存
- 降低 JSON 拷贝
- 更细粒度锁

## 14. 哪些地方不要机械照抄 Go 版

### 不要照抄 1：所有 JSON 都做字符串 patch

Rust 更适合用 `serde_json::Value` 或 typed struct；除非 profiling 证明这里是瓶颈，否则先保持清晰度。

### 不要照抄 2：把所有状态塞进一个大对象里到处传引用

Rust 借用规则会让这种结构很痛苦。建议多用：

- `Arc<Service>`
- 不可变快照
- 小粒度锁

### 不要照抄 3：直接复制 Go 的并发方式

Go 用 channel + goroutine 非常自然；Rust 更适合：

- `tokio::sync::mpsc`
- `Semaphore`
- `JoinSet`
- `CancellationToken`

### 不要照抄 4：用删除文件作为唯一失效策略

Go 版会在某些错误下直接删磁盘账号文件。Rust 版建议把“内存移除”和“磁盘删除”分开抽象，至少让删除策略可配置，否则排查问题很难。

## 15. 一个更 Rust 风格的改进方向

如果不是要求 100% 行为复刻，我建议 Rust 版顺手做这几个改进：

1. 抽象统一上游事件模型
   - 先把 Codex SSE 解析成内部 enum，再转 OpenAI/Claude。
2. 把账号删除策略配置化
   - `remove_on_refresh_failed`
   - `remove_on_quota_invalid`
3. 把协议转换层做成纯函数 + 单元测试优先
4. 给后台任务加指标
   - 刷新成功数、重试次数、冷却账号数
5. 增加结构化日志
   - 使用 `tracing` fields，而不是纯字符串日志

## 16. 建议的第一版目标

如果你想尽快有一个“可用的 Rust 版”，建议先把目标控制在：

- 支持 `/v1/chat/completions`
- 支持流式和非流式
- 支持多账号轮询
- 支持内部重试
- 支持 token 定时刷新
- 支持思考后缀

Claude、quota、compact、health check 都可以放到第二阶段。

## 17. 读完代码后的结论

这个项目最核心的不是 HTTP 框架，而是下面 4 件事：

1. 账号生命周期管理。
2. 请求在成功前的内部重试。
3. 多协议请求/响应转换。
4. SSE 流的稳定处理。

Rust 重写时，如果这 4 个点的边界拆清楚，整体实现会很顺；如果一开始把 handler、JSON 转换、账号状态、网络请求混在一起，后面会很难维护。

## 18. 最后给你的实现建议

一句话版本：

先写“协议转换纯函数 + 执行器重试框架 + 账号状态机”，再接 HTTP，最后补后台任务和高级接口。

如果你照这个顺序做，Rust 版会比现在的 Go 版更容易测、更容易维护，也更容易继续扩展。
