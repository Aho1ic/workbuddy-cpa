# WorkBuddy-CPA 实施计划与架构规范 (IMPLEMENTATION_PLAN)

> **目标**：针对 `/Applications/WorkBuddy AI.app`（Tencent CodeBuddy / WorkBuddy 桌面端生态）底层模型推理能力，通过 CLIProxyAPI (CPA) 插件 C ABI 封装为通用中间件，向任何兼容 OpenAI / Anthropic 协议的第三方客户端（Claude Code、Cursor、Cline、Cherry Studio 等）提供透明、真流式、低延迟的调用能力。当前工程已完成约 50%，本计划为通往 100% 生产就绪的高颗粒度实施路线图。

---

## 0. 逆向探测拓扑与架构依赖图 (Trellis / Topology)

基于对 `/Applications/WorkBuddy AI.app` 逆向探查，以及参考工程 `mimo-cpa` 与 `workbuddy-cliproxy`（见 README 鸣谢链接） 的架构解构，整体拓扑如下：

```mermaid
graph TD
    Client[三方客户端: Claude Code / Cursor / Cline] -->|HTTP OpenAI / Anthropic 协议| CPA[CLIProxyAPI Host Core :8317]
    
    subgraph WorkBuddy_CPA_Plugin ["workbuddy-CPA (c-shared Plugin)"]
        ABI[C ABI Bridge: cliproxyPluginCall] --> Dispatcher[handleMethod RPC 调度器]
        Dispatcher --> AuthMod[凭据与认证管理]
        Dispatcher --> ExecMod[Executor 转发引擎]
        Dispatcher --> ModelMod[模型元数据管理]
        
        AuthMod --> CredLoader[多源凭据导入器]
        CredLoader -.->|只读/防锁拷贝提取 session Cookie| AppSession[~/.workbuddy/app/session/Cookies]
        CredLoader -.->|原子读改写合并| LocalCred[workbuddy.json]
        
        AuthMod --> TokenRefresher[/v2/plugin/auth/token/refresh]
        
        ExecMod --> Rewriter[动态模板改写 rewriteRules & 混元思考深度增强]
        ExecMod --> StreamPump[异步流式泵: SSE Scanner]
        StreamPump -->|首 chunk 前 401 自愈重放| StreamPump
        StreamPump -->|host.stream.emit| HostStream[Host 异步流管道]
    end
    
    CPA -->|dlopen & C ABI| ABI
    HostStream -->|推流| CPA
    
    ExecMod -->|HTTPS POST /v2/chat/completions| Upstream[WorkBuddy AI / CodeBuddy 上游服务]
    
    subgraph Upstream_Realms ["Upstream Realm 集群 (四级判定优先级)"]
        Upstream --> Realm1["International (www.workbuddy.ai)"]
        Upstream --> Realm2["China (www.workbuddy.cn)"]
        Upstream --> Realm3["Enterprise Copilot (copilot.tencent.com)"]
    end
```

### 0.1 关键逆向发现事实库 (Ground Truths)
1. **应用架构与数据层**：
   - 目标 App：`WorkBuddy AI.app` v5.5.2（基于 Electron 37.10.3 / Node 22.21.1）。
   - 用户数据目录：统一位于 `~/.workbuddy` 与 `~/.workbuddy-ai`。
   - Chromium Cookies 存储：实测位于 `~/.workbuddy/app/session/Cookies`，其中 `session` 字段（长度 ~3177 字节）包含免密会话。
   - 内部虽拉起 `cli/bin/codebuddy --serve` 本地 Express 服务（监听动态高位端口如 `127.0.0.1:63177`），但其云端核心模型请求统一收敛至上游 API 网关。
2. **上游 API 契约与风控行为**：
   - 会话端点：`POST {base}/v2/chat/completions`。
   - 必须强制流式：`stream: true`；若传非流式，上游直接返回业务拦截码 `11101`。
   - 认证端点：`/v2/plugin/auth/state`、`/v2/plugin/login/account`、`/v2/plugin/auth/token`、`/v2/plugin/auth/token/refresh`。
   - 敏感模板过滤：上游对 Claude Code 的固化 System Prompt（如 `"You are Claude Code, Anthropic's official CLI for Claude."` 与 `"Main branch (you will usually use this for PRs)"`）做精确匹配拉黑，必须在线做同义微改写。
   - 混元思考模型限制：`hy3` 系列模型仅在 `reasoning_effort="high"` 时生效深度思考，其余参数会被降级为无思考。
3. **架构与设计审查仲裁结论 (Grill-me Decisions)**：
   - **协议分界**：插件层聚焦标准化 OpenAI 流输出，并针对非 `/v1/chat/completions` 请求补足 `data: ` SSE 封帧，完整复用 CPA 宿主成熟的双向协议转译层。
   - **路由优先级**：严格四级仲裁：`环境变量 WORKBUDDY_BASE_URL > workbuddy.json 明确指定 > Cookies 提取的 domain > 默认 fallback (www.workbuddy.ai)`。
   - **模板改写**：规则收敛在 `workbuddy.json` 的 `rewriteRules` 节点，支持用户热配置扩展，内置默认硬编码规则作为 fallback。
   - **流式 401 自愈**：在向宿主发送首个 chunk 之前，若上游返回 401，立即加锁刷新 Token 并无缝重放请求，实现对客户端零感知自愈。
   - **Cookies 免锁提取**：以 `file:...mode=ro&immutable=1` 只读打开，遇锁冲突自动复制副本至 `/tmp` 解析，保障桌面端开启时无缝导入。
   - **模型发现**：10 款核心模型静态白名单兜底 + 凭据就绪后运行时探活 `/v2/enterprises/personal/models` 动态合并。
   - **配置原子写入**：内存互斥锁 + 读-改-写深层合并 + 临时文件原子替换（`os.Rename`），保障并发安全且绝不丢失用户配置。
   - **构建平台**：聚焦 macOS arm64（Apple Silicon），采用 `-trimpath` 编译与本地 ad-hoc 签名。

---

## 1. 阶段划分与文件级实施任务 (File-level Tasks)

---

### Phase 1: 逆向/探测与上游通信协议解耦

**核心目标**：实现对 WorkBuddy 多环境 Realm 智能适配、全自动无缝提取本地桌面端凭据，以及完善的 Token 自动刷新与并发写安全保障。

#### Task 1.1: 桌面端凭据嗅探器升级（多路径与 Session Cookie 智能无锁导入） [COMPLETED]
- **Target File**: `scripts/import_desktop_auth.py`
- **Input**:
  - `~/.workbuddy/app/session/Cookies`
  - `~/.workbuddy-ai/app/session/Cookies`
  - 现存 `workbuddy.json`（若已存在需做合并保护）
- **Action**:
  1. 在 `CANDIDATE_DBS` 中新增 `~/.workbuddy/app/session/Cookies` 和 `~/.workbuddy-ai/app/session/Cookies`。
  2. 实现免锁读取逻辑：使用 `file:{db}?mode=ro&immutable=1` URI 打开；若捕获 `sqlite3.OperationalError: database is locked`，自动拷贝数据库及其 WAL 到 `/tmp/wb_cookies_temp.db` 进行离线读取，读后安全清理。
  3. 针对 `name='session'` 的长 Cookie，提取绑定的域名（`www.workbuddy.cn` / `www.workbuddy.ai` / `copilot.tencent.com`）。
  4. 支持与既有 `workbuddy.json` 深度合并：若文件中已存在 `rewriteRules` 或自定义 `account` 配置，保持字段不被覆写。
- **Acceptance Criteria**: [VERIFIED]
  - 在 WorkBuddy AI 桌面端运行状态下运行导入命令：
    ```bash
    python3 scripts/import_desktop_auth.py -o workbuddy.json
    ```
  - 验证生成的 `workbuddy.json` 具备有效凭据且文件权限为 `0600`，命令返回码为 0。

#### Task 1.2: Realm 智能路由与网关 Header 动态生成 [COMPLETED]
- **Target File**: `main.go` (`baseURLFor`, `originForHost`, `backendHeaders`)
- **Input**:
  - `storedAuth` 结构体中的 `domain`, `baseURL`, `account.enterpriseId`。
  - 环境变量 `WORKBUDDY_BASE_URL`。
- **Action**:
  1. 完善 `baseURLFor` 解析逻辑，严格执行四级优先级：
     - Step 1: 检查 `os.Getenv("WORKBUDDY_BASE_URL")`，非空直接作为 base。
     - Step 2: 检查凭据中 `sa.BaseURL`，非空则作为 base。
     - Step 3: 检查凭据中 `sa.Auth.Domain`，若有映射为其标准 HTTPS 形式。
     - Step 4: 回退至默认 `https://www.workbuddy.ai`。
  2. 完善 `backendHeaders`：
     - 根据解析出的 Host 动态配置 `Origin` 与 `Referer`（`originForHost`）。
     - 注入 `Authorization: Bearer <accessToken>` 与 `X-Access-Token: <accessToken>`。
     - 注入 `User-Agent: CLI/2.63.2 CodeBuddy/2.63.2`、`X-Auth-Refresh-Source: workbuddy`。
     - 若 `sa.Account.EnterpriseID` 非空，注入 `X-Enterprise-Id`。
- **Acceptance Criteria**: [VERIFIED]
  - 单元测试覆盖环境变量覆盖、凭据 domain、企业域 header 逻辑：
    ```bash
    go test -v -run TestRealmRoutingAndHeaders ./...
    ```

#### Task 1.3: Token 续期防抖、并发互斥与原子持久化 [COMPLETED]
- **Target File**: `main.go` (`handleRefreshAuth`, `saveStoredAuthAtomic`, `refreshAuthLock`)
- **Input**:
  - `/v2/plugin/auth/token/refresh` 端点规范。
  - 凭据文件 `workbuddy.json`。
- **Action**:
  1. 引入全局互斥锁 `var refreshMu sync.Mutex`，防止多个并发请求同时发起重复续期。
  2. 引入 5 分钟提前续期判定（Skew Buffer）：请求前若 `sa.Auth.ExpiresAt > 0 && time.Now().Unix() + 300 >= sa.Auth.ExpiresAt`，自动先刷新。
  3. 实现 `saveStoredAuthAtomic(sa *storedAuth, path string) error`：
     - 读取磁盘既有 JSON，利用 `map[string]any` 进行深度合并，保留用户的 `rewriteRules` 等非认证自定义字段。
     - 写入临时文件 `workbuddy.json.tmp`。
     - 调用 `os.Rename` 原子覆盖目标文件，确保断电或中断不损坏配置。
- **Acceptance Criteria**: [VERIFIED]
  - 并发刷新测试与原子保存测试全绿：
    ```bash
    go test -v -run TestConcurrentRefreshAndAtomicSave ./...
    ```

---

### Phase 2: CLIProxyAPI 核心代理层搭建与双协议流式适配

**核心目标**：提供高质量 OpenAI / Anthropic 协议双向互转支持，完善真流式 SSE 泵、思考字段（Reasoning/Thinking）映射、Tool Calls 折叠与过滤。

#### Task 2.1: Anthropic Messages 协议与 OpenAI Chat Completions 协议对齐 [COMPLETED]
- **Target File**: `main.go` (`clientNeedsSSEFrame`, `cleanChunkJSON`, `forceMaxThinking`)
- **Input**:
  - CPA Host 转发的 `ExecutorRequest.Metadata["request_path"]` 与原始 Payload。
- **Action**:
  1. 完善跨协议流式分帧判定：当请求来自 `/v1/messages` 等非原生 OpenAI 路由时，`clientNeedsSSEFrame` 必须返回 `true`，以在 chunk 前追加 `data: ` 供 CPA 转译层分帧。
  2. 思考链路深度对齐：
     - 上游返回 `delta.reasoning_content`（混元 / DeepSeek 风格）。
     - 对入参中 `thinking` 字段进行识别；若模型为 `hy3*` 系列，强制注入 `reasoning_effort: "high"`，确保混元大模型激活最大深度思考能力。
- **Acceptance Criteria**: [VERIFIED]
  - 针对非流式与流式数据帧净化的单测通过：
    ```bash
    go test -v -run TestCleanChunkJSONAndReasoning ./...
    ```

#### Task 2.2: Tool Calling (函数调用) Delta 增量聚合与安全防御 [COMPLETED]
- **Target File**: `main.go` (`aggregateCompletion`, `cleanChunkJSON`)
- **Input**:
  - 上游流式返回 choices 数组内的 `delta.tool_calls`。
- **Action**:
  1. 在 `cleanChunkJSON` 中，对空数组 `tool_calls: []` 或 `function_call: null` 进行剥离，防止部分三方严苛解析器（如严苛 JSON Schema 验证器）反序列化报错。
  2. 在非流式折叠器 `aggregateCompletion` 中，实现多 tool_calls 增量聚合（按 `index` 聚合 `name`、`arguments` 字符串碎片），产出标准 `choices[0].message.tool_calls` 结构。
- **Acceptance Criteria**:
  - 包含流式分片 Tool Call 碎片的合并测试用例全绿：
- **Acceptance Criteria**: [VERIFIED]
    go test -v -run TestAggregateToolCalls ./...
    ```

#### Task 2.3: 动态可扩展模板过滤系统 (Dynamic Cat-and-Mouse Rules)
#### Task 2.3: 动态可扩展模板过滤系统 (Dynamic Cat-and-Mouse Rules) [COMPLETED]
- **Input**:
  - Claude Code 注入的 System Prompts。
  - `workbuddy.json` 中配置的 `"rewriteRules": [{"pattern": "...", "replacement": "..."}]`。
- **Action**:
  1. 定义 `RewriteRule` 结构体，支持从 `workbuddy.json` 动态加载用户规则。
  2. 内置默认兜底规则：
     - `"You are Claude Code, Anthropic's official CLI for Claude."` → `"You are Claude Code, Anthropic's official CLI tool for Claude."`
     - `"Main branch (you will usually use this for PRs)"` → `"Default branch (you will usually use this for PRs)"`
  3. 支持深层递归扫描多模态消息数组（`messages[].content` 中的 `text` 分块），对文本进行正规化微调替换。
- **Acceptance Criteria**:
- **Acceptance Criteria**: [VERIFIED]
    ```bash
    go test -v -run TestDynamicRewriteRules ./...
    ```

---

### Phase 3: 本地服务生命周期管理与守护进程/插件集成

**核心目标**：提供无缝的编译构建体系、macOS arm64 架构专属编译与签名、模型动态发现与健康检查。

#### Task 3.1: 自动化构建与 macOS Ad-hoc 签名 [COMPLETED]
- **Target File**: `Makefile`
- **Input**:
  - Go 编译器、Clang、macOS `codesign`。
- **Action**:
  1. 配置默认构建目标为当前 Apple Silicon arm64 架构：
     `CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags "-X main.pluginVersion=0.2.0" -o workbuddy.dylib .`
  2. 构建完成后自动调用 `codesign --force --sign - workbuddy.dylib`，确保 macOS Gatekeeper 正常加载。
  3. 保留对 Linux `.so` 目标的构建兼容能力（`make PLUGIN=workbuddy.so`）。
- **Acceptance Criteria**: [VERIFIED]
  - 执行 `make smoke` 一键完成编译、C 宿主驱动调用并验证 C ABI 契约：
    ```bash
    make clean && make smoke
    ```
  - 终端打印 `SMOKE PASS`，退出码为 0。

#### Task 3.2: 模型元数据发现（静态兜底 + 动态探测合并） [COMPLETED]
- **Target File**: `main.go` (`wbModels`, `syncUpstreamModels`)
- **Input**:
  - 10 款内置静态核心模型配置。
  - 上游端点 `GET {base}/v2/enterprises/personal/models`。
- **Action**:
  1. 初始保持 10 款精选模型静态列表以供瞬时响应。
  2. 在后台或收到鉴权信息后，异步发起探活拉取用户账号实际开通的模型列表，并动态合并至可用模型清单中。
  3. 上报标准 `pluginapi.ModelInfo` 列表。
- **Acceptance Criteria**: [VERIFIED]
  - 模拟上游返回企业自定义模型列表的测试通过，动态合并入清单：
    ```bash
    go test -v -run TestDynamicModelDiscovery ./...
    ```

#### Task 3.3: 部署与客户端配置模板封装 [COMPLETED]
- **Target File**: `config/cpa_config.yaml.example`, `scripts/run_cpa.sh`
- **Input**:
  - 编译产物 `workbuddy.dylib`。
  - CLIProxyAPI 标准配置规范。
- **Action**:
  1. 编写完整的 `config/cpa_config.yaml.example`，说明如何配置 CPA 加载 `workbuddy.dylib` 插件并映射路由。
  2. 编写启动辅助脚本 `scripts/run_cpa.sh`，自动检测插件路径与凭据文件，并带环境变量启动 CPA。
- **Acceptance Criteria**: [VERIFIED]
  - 脚本具备执行权限并通过语法检查：
    ```bash
    bash -n scripts/run_cpa.sh
    ```

---

### Phase 4: 端到端兼容性验证与异常容错机制

**核心目标**：实现首 chunk 前 401 透明重试自愈、客户端断开取消传播（Client Abort Handling）、上游错误代码标准化映射与 E2E 自动化测试。

#### Task 4.1: 流式读取首 chunk 前 401 自动重试与断连资源释放 [COMPLETED]
- **Target File**: `main.go` (`pumpUpstreamStream`)
- **Input**:
  - Host 异步流管道 `hostCall(pluginabi.MethodHostStreamEmit)`。
  - 上游 HTTP 响应状态码。
- **Action**:
  1. 零感知 401 自愈：在 `pumpUpstreamStream` 中，如果上游在返回第 1 个数据行前响应 HTTP 401，且未曾向 host emit 任何数据，立即加锁调用刷新逻辑更新 Token，并自动重试发起当前请求；若重试仍失败，再向 host emit 错误。
  2. 连接生命周期管理：当 `streamEmit` 返回错误（客户端已中途关闭连接/离开），立即终止上游 scanner 循环并关闭 `resp.Body`，防止后台 goroutine 悬挂与上游网络资源泄漏。
- **Acceptance Criteria**: [VERIFIED]
  - 模拟首包 401 触发自动刷新并恢复推流的测试全绿：
    ```bash
    go test -v -run TestStreamPump401SelfHealing ./...
    ```

#### Task 4.2: 上游业务异常代码标准化拦截与归一化 [COMPLETED]
- **Target File**: `main.go` (`normalizeUpstreamError`)
- **Input**:
  - 上游返回的业务 JSON：`{"code": 11101, "msg": "...", "data": null}` 或 HTTP 4xx/5xx。
- **Action**:
  1. 拦截非 200 HTTP 响应或 `code != 0` 的业务异常：
     - `11101`: 提示上游仅接受流式请求（强制纠偏并记录日志）。
     - `10003` / `401`: Token 失效错误（映射为标准 HTTP 401 / `invalid_api_key`）。
     - `429`: 限流错误（映射为标准 HTTP 429 / `rate_limit_exceeded`）。
  2. 输出符合 OpenAI 标准的错误结构体：
     `{"error": {"message": "...", "type": "upstream_error", "code": "..."}}`。
- **Acceptance Criteria**: [VERIFIED]
  - 异常映射单测覆盖 401、429、11101 转换：
    ```bash
    go test -v -run TestNormalizeUpstreamError ./...
    ```

#### Task 4.3: 端到端流式请求集成验证脚本 (E2E Smoke Script) [COMPLETED]
- **Target File**: `scripts/e2e_verify.sh`
- **Input**:
  - 运行中的 CPA 服务（端口如 `8317`）或 Mock Server。
- **Action**:
  1. 编写完整的 curl 自动化集成验证脚本：
     - 验证 1：OpenAI 协议流式请求：
       ```bash
       curl -N -s -X POST http://127.0.0.1:8317/v1/chat/completions \
         -H "Content-Type: application/json" \
         -H "Authorization: Bearer test" \
         -d '{"model":"glm-5.2","messages":[{"role":"user","content":"ping"}],"stream":true}'
       ```
     - 验证 2：OpenAI 协议非流式请求（验证折叠器）：
       ```bash
       curl -s -X POST http://127.0.0.1:8317/v1/chat/completions \
         -H "Content-Type: application/json" \
         -H "Authorization: Bearer test" \
         -d '{"model":"glm-5.2","messages":[{"role":"user","content":"ping"}],"stream":false}'
       ```
     - 验证 3：Anthropic 协议请求（验证 `/v1/messages` 兼容性）。
  2. 脚本自动断言响应中包含 `chat.completion` 或 `data:` 流式切片，且 HTTP 状态码为 200。
- **Acceptance Criteria**: [VERIFIED]
  - 脚本执行输出全部 PASS，退出码为 0。

---

## 2. 实施甘特进度与验收矩阵

| 任务编号 | 任务名称 | 目标文件 | 核心交付成果 | 验证命令 |
| :--- | :--- | :--- | :--- | :--- |
| **T1.1** | 桌面端凭据免锁嗅探器 [DONE] | `scripts/import_desktop_auth.py` | 支持只读/拷贝免锁解析 `~/.workbuddy/app/session/Cookies` | `python3 scripts/import_desktop_auth.py -o workbuddy.json` |
| **T1.2** | Realm 智能路由与网关 Header [DONE] | `main.go` | 严格四级优先级判定 + 企业域与指纹 Header | `go test -v -run TestRealmRoutingAndHeaders ./...` |
| **T1.3** | Token 续期防抖与原子写入 [DONE] | `main.go` | 互斥锁 + 5分钟提前续期 + 临时文件原子替换覆盖 | `go test -v -run TestConcurrentRefreshAndAtomicSave ./...` |
| **T2.1** | Messages 双协议流式适配 [DONE] | `main.go` | `clientNeedsSSEFrame` 智能分帧 + 混元强制高深度思考 | `go test -v -run TestCleanChunkJSONAndReasoning ./...` |
| **T2.2** | Tool Calling 增量聚合与过滤 [DONE] | `main.go` | 工具调用分片清洗与完整结构折叠 | `go test -v -run TestAggregateToolCalls ./...` |
| **T2.3** | 动态模板改写规则扩展 [DONE] | `main.go` | 配置文件 `rewriteRules` 支持 + 多模态扫描 | `go test -v -run TestDynamicRewriteRules ./...` |
| **T3.1** | 自动化构建与 Ad-hoc 签名 [DONE] | `Makefile` | 产出合规 `workbuddy.dylib` 并通过 C 宿主冒烟 | `make clean && make smoke` |
| **T3.2** | 动态模型元数据发现 [DONE] | `main.go` | 静态白名单兜底 + 运行时拉取上游接口动态合并 | `go test -v -run TestDynamicModelDiscovery ./...` |
| **T3.3** | CPA 配置与一键运行脚本 [DONE] | `config/`, `scripts/run_cpa.sh` | 完整的 CPA 挂载配置与启动器 | `bash -n scripts/run_cpa.sh` |
| **T4.1** | 流式首 chunk 前 401 透明自愈 [DONE] | `main.go` | 首包前 401 自动续期重发 + 客户端断开连接及时释放 | `go test -v -run TestStreamPump401SelfHealing ./...` |
| **T4.2** | 上游业务错误标准化拦截 [DONE] | `main.go` | 11101 / 401 / 429 规范映射为 OpenAI 错误 | `go test -v -run TestNormalizeUpstreamError ./...` |
| **T4.3** | 端到端集成自动化验证 [DONE] | `scripts/e2e_verify.sh` | 流式/非流式/双协议 E2E 测试断言闭环 | `bash scripts/e2e_verify.sh` |

---

## 3. 安全与隔离约束 (Safety & Non-goals)

1. **凭据安全**：
   - 提取的 `workbuddy.json` 包含机密 Token，**严禁提交入 Git**（已纳入 `.gitignore`）。
   - 脚本生成凭据文件时自动赋予 `0600` 权限。
2. **只读保护**：
   - 读取桌面端 SQLite 数据库时，强制采用 `file:...mode=ro&immutable=1` 只读 URI 方式，若遇锁直接复制离线副本，严禁修改桌面应用自身数据库。
3. **侵入性最小化**：
   - 不修改 `/Applications/WorkBuddy AI.app` 内部任何文件（asar / 签名保持原始完整），纯外部外挂解耦。
