# workbuddy-CPA

把 **WorkBuddy AI.app**（`/Applications/WorkBuddy AI.app`，默认上游 `www.workbuddy.ai`）背后的模型封装成
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA) 插件。任何支持 OpenAI / Anthropic 协议的客户端
（Claude Code、Cursor、Cline、Cherry Studio、SDK……）都能通过 CPA 直接调用 WorkBuddy 账号可用的模型。

实现方式对齐 [workbuddy-cliproxy](https://github.com/lovingfish/workbuddy-cliproxy) 与
[mimo-cpa](https://github.com/Aho1ic/mimo-cpa)：以 `-buildmode=c-shared` 导出 cliproxy C ABI，注册 `workbuddy`
provider，负责凭据解析 / 登录 / token 刷新，并把 chat completions 转发到 WorkBuddy 同款上游
`/v2/chat/completions`。

## 工作原理

```text
客户端 (OpenAI / Anthropic 协议)
        │  ANTHROPIC_BASE_URL=http://127.0.0.1:8317
        ▼
   CLIProxyAPI (CPA)  ── 加载 workbuddy.dylib / workbuddy.so
        │  plugin.register / auth.* / executor.execute(_stream)
        ▼
   本插件 (Go c-shared)
     ├─ 凭据: workbuddy.json（登录流产出 / 手填 / 桌面导入脚本）
     ├─ realm: 默认 https://www.workbuddy.ai
     │          凭据里 domain / baseURL 可切 www.workbuddy.cn / copilot.tencent.com
     └─ 转发: POST <base>/v2/chat/completions（真流式, host.stream.emit 逐 chunk 推送）
```

## 构建与验证

需要 Go（≥1.26，cgo）与 clang：

```bash
make test    # go vet + 单元测试（凭据归一化、realm 解析、模板改写、hy3 强思考）
make build   # 产出 workbuddy.dylib（Linux 为 workbuddy.so）+ 本地 ad-hoc 签名
make smoke   # C 真宿主冒烟: dlopen 插件, 驱动 C ABI 断言信封契约
```

## 安装

1. 把 `workbuddy.dylib`（或 `.so`）放进 CPA 的插件目录（如 `plugins/`，随 CPA 版本略有差异）。
2. 提供凭据（三选一，见下），CPA 面板里出现 `workbuddy` provider 即加载成功。
3. 客户端指向 CPA（默认 `http://127.0.0.1:8317`），例如 Claude Code：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export ANTHROPIC_API_KEY='<CPA 里配置的 key>'
export ANTHROPIC_MODEL='hy3'          # 或 glm-5.2 / kimi-k2.7 / deepseek-v4-pro 等
claude
```

OpenAI 协议客户端同理指向 `http://127.0.0.1:8317/v1`。

## 凭据来源（三选一）

**A. 插件登录流**：CPA 触发 `auth.login.start`，浏览器扫码/账号登录后 `auth.login.poll` 收回
accessToken/refreshToken，自动持久化为 `workbuddy.json`。登录基址默认 `www.workbuddy.ai`；CN 账号在
login.start 请求里带 `{"realm":"cn"}` 或 `{"base_url":"https://www.workbuddy.cn"}` 切换。

**B. 手填**：参照 [`workbuddy.json.example`](workbuddy.json.example)。兼容三种形状：
canonical `{"auth":{"accessToken","refreshToken"},"account":{...}}`、
flat `{"accessToken","refreshToken","domain","baseURL","uid"}`、
API key `{"type":"workbuddy","mode":"api_key","apiKey":"..."}`。

**C. 从 WorkBuddy AI.app 桌面端导入**：

```bash
python3 scripts/import_desktop_auth.py            # 自动探测 app 的 Cookies 库
python3 scripts/import_desktop_auth.py --db <Cookies路径> -o workbuddy.json
```

脚本只读探测 `~/Library/Application Support/WorkBuddy*` 下的 Chromium Cookies 库，提取
workbuddy/codebuddy 域的 token cookie 生成 `workbuddy.json` 草稿。若 cookie 值是 Chromium 加密
（`v10`+ 前缀）无法直接解密，脚本会列出候选 cookie 名（值打码）供手工映射。凭据是秘密，
`workbuddy.json` 已在 `.gitignore`，不要提交。

## 模型与转发行为

- 静态列表：`glm-5.2` · `glm-5.1` · `glm-5v-turbo` · `kimi-k2.7` · `minimax-m3-pay` · `hy3` ·
  `hy3-preview` · `hy3-preview-agent` · `deepseek-v4-pro` · `deepseek-v4-flash`（实际可用性以账号权限为准）。
- **渠道校验（channel verification）**：上游对 `/v2/chat/completions` 校验请求头里的会话身份族
  `X-Conversation-ID` / `X-Conversation-Request-ID` / `X-Conversation-Message-ID`（官方 CLI 每请求重新生成）。
  三个头缺任意一个，整条请求被网关拒为 `HTTP 400 {"code":11128,"msg":"request illegal"}`，
  `displayTips` 明说 "failed channel verification, possibly because it was not sent from an official client"。
  插件在 `backendHeaders` 里逐请求补发这三个 UUID。UA / `X-IDE-*` / `X-Agent-*` / `x-stainless-*` 头族
  实测**不是**该闸门的必要条件（2026-10-20 排除矩阵验证）。
- **模板改写**：上游对 Claude Code 的两句固定 system 模板做精确匹配拉黑；转发前自动最小改写
  （`CLI`→`CLI tool`、`Main branch`→`Default branch`），语义不变。属 cat-and-mouse，上游加句需跟进。
- **hy3 系列强制思考**：`hy3` / `hy3-preview` / `hy3-preview-agent` 转发前强制 `reasoning_effort=high`，
  覆盖客户端设置（上游只认 `high`）。其余模型 `reasoning_effort` 原样透传。
- **真流式**：上游只接受流式请求（非流式回 code 11101），插件始终按流式拉上游；有 `stream_id` 时经
  `host.stream.emit` 逐 chunk 推送，否则收集 chunk 折叠为单个 chat.completion。思考内容走
  `delta.reasoning_content`。

## 已知限制与验证状态

- 已验证：`make test`（含负控）与 `make smoke`（真实 C 宿主驱动完整 C ABI）全绿。
- 已验证（2026-10-20，真实账号 + CPA）：OpenAI `/v1/chat/completions`（流式 / 非流式折叠）与 Anthropic
  `/v1/messages` 均正常返回；`tools` 请求正常。该账号实测可用模型：`deepseek-v4.1-flash` ·
  `deepseek-v4.1-flash-sg` · `glm-5.3` · `glm-5.3-flash` · `glm-5.2` · `glm-5.1` · `glm-5v-turbo` ·
  `kimi-k3` · `kimi-k2.8-preview` · `kimi-k2.7` · `kimi-k2.6` · `minimax-m3` · `hy4-preview` ·
  `hy4-preview-f` · `hy3`；返回 `11102 model [...] service info not found`（即该 ID 无权限/不存在）的有：
  `deepseek-v4-flash` · `deepseek-v4-pro` · `minimax-m3-pay` · `hy3-preview` · `hy3-preview-agent`。
  静态列表是跨账号超集，客户端应以上游 `code 11102` 为准切换模型。
- 部分未验证：`/v2/plugin/*` 登录面与 `/v2/enterprises/personal/models` 等端点形状来自公开参考实现（见下），
  实际以上游响应为准；若 WorkBuddy AI.app 更新了鉴权面，先用 `curl -v` 打一次上游确认再改 `path*` 常量。
- 账号侧：token 过期后插件走 `/v2/plugin/auth/token/refresh` 自动刷新；刷新失败需重新登录或重导凭据。

## 致谢

- [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) — workbuddy 插件原始设计。
- [lovingfish/workbuddy-cliproxy](https://github.com/lovingfish/workbuddy-cliproxy) — 本项目代码基底
  （clean-room 重写版，MIT），C ABI 与转发架构。
- [Aho1ic/mimo-cpa](https://github.com/Aho1ic/mimo-cpa) — 桌面凭据导入模式与工程布局（MIT）。
- [aiden-git/wb2cpa](https://github.com/aiden-git/wb2cpa) — WorkBuddy 国际版 realm / 端点事实参考（MIT）。

## License

MIT（见 [LICENSE](LICENSE)，沿用 workbuddy-cliproxy 版权声明）。
