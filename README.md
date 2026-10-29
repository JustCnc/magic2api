# magic2api — 超级麦吉（Magic）模型网关 → CLIProxyAPI 插件

把超级麦吉桌面端（`Magic` 0.0.19）使用的模型网关接进 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)，
对外提供 OpenAI 兼容接口（`/v1/chat/completions`、`/v1/models`），并附带账号池、登录、积分、请求日志和内嵌控制台。

- 上游模型网关：`https://i-magic-service.letsmagic.cn/v1/*`（`api-key: mgw_...` 鉴权，原生 OpenAI 协议）
- 上游资产/账号网关：`https://i-magic-service.letsmagic.cn/api/v1/*`（`authorization` + `organization-code`）
- 上游账号服务：`https://i-keewood-service.teamshare.cn/v4/*`、`/captcha/*`
- 逆向结论与证据：[`docs/REVERSE_ENGINEERING.md`](docs/REVERSE_ENGINEERING.md)

## 特性

| 能力 | 说明 |
| --- | --- |
| 对话代理 | 流式 / 非流式、工具调用、`reasoning_content`、`usage` 原样透传；上游是原生 OpenAI 协议，插件只重写 `model` |
| 模型目录 | 从 `GET /v1/models` 拉取并缓存，对外暴露为 `magic/<上游 id>`；用 `/api/v1/platform/models` 补展示名、上下文与能力 |
| 账号池 | 配置主账号 + `accounts_dir/*.json` + 控制台登录/导入 + CPA 凭据导入；每账号独立 `device_id` |
| 令牌链 | `authorization`(JWT 30d) → `api-key`(2h) + `refresh_token`(7d)；到期自动 `PUT` 续期，失败回退重新铸造，并发去重 |
| 限流 | 每账号本地令牌桶（默认 30 rpm / burst 20），超限返回本地 429 让宿主换号 |
| 冷却与轮换 | 未冷却 > 优先级高 > 最近成功；429/5xx 冷却，鉴权失败冷却，`2155 用户不存在` 这类直接判死 |
| 积分 | 余额、流水、每日登录积分、新人任务奖励；请求结束后回查流水把消耗回填到请求日志 |
| 控制台 | 单文件 `go:embed` HTML + 原生 JS，走 `/v0/management/magic2api/*`，不占额外端口 |

## 安装

1. 从 Releases 下载对应平台的压缩包：
   - Windows：`magic2api_<version>_windows_amd64.zip` → 解压出 `magic2api.dll`
   - Linux：`magic2api_<version>_linux_amd64.zip` / `_linux_arm64.zip` → `magic2api.so`
2. 放进 CPA 的插件目录：

   ```
   <cpa>/plugins/windows/amd64/magic2api.dll     # Windows
   <cpa>/plugins/linux/amd64/magic2api.so        # Linux
   ```
3. 在 CPA 配置里启用（`config.yaml`）：

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       magic2api:
         enabled: true
         priority: 10
         data_dir: "plugin-data/magic2api"
         # 可选：直接放主账号，省去控制台登录
         # authorization: "eyJhbGciOi..."
         # organization_code: "h70imn51gp0f"
   ```
4. 重启 CPA（或让它重载配置），然后在 CPA 控制台菜单里打开 **Magic2API** 页签。

## 拿到登录态

三种方式，任选：

1. **控制台「导入登录态」**：粘贴 JWT（只填 `authorization` 也行，插件会自己问上游要组织编码）。
2. **控制台手机号登录**：手机号 + 密码（走官方 `/v4/user/login`，无需验证码）；或短信登录（官方旋转图形验证码，插件已实现角度混淆算法）。
3. **桌面端导出脚本**（Node，无依赖）：

   ```bash
   node scripts/export_credentials.js > account.json
   ```

   脚本扫描 `%APPDATA%\Magic\Local Storage\leveldb`（明文 LevelDB，没有 DPAPI/AES 加密），
   取出 `MAGIC:user:authorization` 与 `organizationCode`，输出可直接粘贴/放进 `accounts_dir` 的 JSON。

> JWT 有效期 30 天，**上游没有刷新接口**，过期只能重新登录或重新导出。

## 控制台

菜单入口：CPA 控制台 → **Magic2API**（`/v0/management/magic2api/ui`）。页签：

- **概览**：账号数/可用数、模型数、请求量、限流次数、token 与积分累计、目录来源与刷新时间
- **账号**：登录/导入区 + 账号表（邮箱/昵称、组织、设备名与设备码、优先级、api-key 状态、冷却、错误）
- **模型**：对外 id / 上游 id / 展示名 / 上下文 / 最大输出，可手动重拉
- **请求**：时间、账号、模型、流式、状态、首字延迟、总耗时、输入/输出/缓存 token、回填积分、错误
- **积分**：余额卡片、今日登录奖励/任务奖励/模型消耗、任务板（单个领取 + 一键领取）、流水表
- **日志**：插件运行日志（info/warn/error）
- **设置**：rpm、burst、冷却、超时、换号次数、代理、积分回填、自动领积分、调试日志
- **测试对话**：选模型直接打一发，显示账号、耗时、usage 与正文

所有列表容器固定最大高度并独立滚动。管理接口失败会在右下角 toast，不会静默。

## 管理接口

前缀 `/v0/management/magic2api`，除 `GET /ui` 外均返回 `{"ok":true,"data":...}` 或 `{"ok":false,"error":"..."}`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/ui` | 控制台页面 |
| GET | `/overview` | 概览数据 |
| GET | `/accounts` | 账号列表 |
| POST | `/accounts/add` | 添加账号（`{authorization, organization_code?, alias?, priority?}`） |
| POST | `/accounts/delete` | 删除账号（`{id}`） |
| POST | `/accounts/update` | 改优先级/启停/复活/备注（`{id, priority?, disabled?, dead?, note?}`） |
| POST | `/accounts/refresh` | 强制续期 api-key（`{id}`） |
| GET | `/models` | 模型目录 |
| POST | `/models/refresh` | 重新拉取模型目录 |
| GET | `/requests` | 请求日志（最新 200 条） |
| GET | `/points?id=` | 指定账号的余额/任务/流水 |
| POST | `/points/claim` | 领取每日登录积分（`{id}`） |
| POST | `/points/task` | 领取单个新人任务（`{id, code}`） |
| GET | `/logs` | 运行日志 |
| GET / POST | `/settings` | 读取 / 保存运行参数 |
| POST | `/login/password` | 手机号密码登录（`{phone, password, state_code?, alias?}`） |
| POST | `/login/captcha` | 取图形验证码（`{phone}` → `{token, image}`） |
| POST | `/login/sms` | 校验图形验证码并发短信（`{phone, captcha_token, angle}`） |
| POST | `/login/sms/verify` | 短信验证码登录（`{phone, code, alias?}`） |
| POST | `/login/token` | 导入登录态（`{authorization, organization_code?, alias?}`） |
| POST | `/test/chat` | 测试对话（`{model, prompt, max_tokens}`） |

## 配置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `data_dir` | `./plugin-data/magic2api` | `accounts.json`、`models.json` 落盘位置 |
| `authorization` / `organization_code` | 空 | 主账号，两者必须同时给 |
| `device_id` / `device_name` | 自动生成 | 覆盖主账号设备标识 |
| `credential_alias` / `credential_priority` | `default` / `10` | 主账号别名与优先级 |
| `accounts_dir` | 空 | 额外账号目录，每个 `*.json` 一个账号，文件名即别名；删文件即移出池子 |
| `base_url` | `https://i-magic-service.letsmagic.cn` | 模型网关 + 资产网关 |
| `account_base_url` | `https://i-keewood-service.teamshare.cn` | 账号服务（登录/验证码/user-info） |
| `model_prefix` | `magic` | 对外模型命名空间 |
| `model_denylist` | 空 | 按上游 id 隐藏模型 |
| `refresh_seconds` | `1800` | 模型目录刷新间隔 |
| `rpm` / `burst` | `30` / `20` | 单账号本地令牌桶 |
| `cooldown_seconds` | `60` | 可重试错误后的冷却 |
| `request_timeout_seconds` | `300` | 单次上游超时 |
| `max_attempts` | `3` | 一次请求最多换几个账号 |
| `points_logging` | `true` | 请求结束后回查流水回填积分 |
| `daily_points_enabled` | `true` | 自动领每日登录积分与新人任务 |
| `proxy_url` | 空 | 出站 HTTP 代理 |
| `debug_dump` | `false` | 日志里打印请求/响应片段 |

## 构建

```bash
# 本机（Windows，需要 mingw-w64 的 gcc 在 PATH 里）
./build.sh                      # → dist/windows/amd64/magic2api.dll

# 交叉
./build.sh linux/amd64
./build.sh all

# 校验
gofmt -l .
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
```

发布由 GitHub Actions 完成：推送 tag `v<pluginVersion>`（版本号唯一来源是 `schema.go` 的 `pluginVersion`），
`release.yml` 会校验 tag 与 `pluginVersion` 一致，构建 `linux/amd64`、`linux/arm64`、`windows/amd64`，
产出 `magic2api_<version>_<goos>_<goarch>.zip` 与 `checksums.txt` 并上传到 Release。

## 实现要点（与上游对齐）

- **两种鉴权不能混**：模型网关只认 `api-key`，资产接口只认 `authorization` + `organization-code`；
  混用会得到 HTTP 200 + `{"code":403,"message":"token无效"}`（注意是 200，不是 401）。
- **业务错误码在 200 里**：资产接口成功码是 `1000`，鉴权类 `2179/3103/2155`，登录类 `2152/2168`，
  新人任务 `9303`（不可手动完成）/`9304`（过期）/`9306`/`9307`（可重试）。
- **宽松 JSON**：`amount` 是字符串（`"-68"`），`user_id` 有时是数字，雪花 id 超过 float64 精度，
  所以用 `flexFloat/flexString/flexID` 解析，避免整包解析失败。
- **非 JSON 响应**：WAF 页/空 body 会在同一请求内换连接重试一次，错误信息带 HTTP 状态、Content-Type 与 body 片段。
- **流式**：插件只把上游 SSE 的 JSON 帧交给宿主，`data: ` 前缀与 `[DONE]` 由宿主补；上游没有 `finish_reason`
  时插件补一个收尾帧，否则宿主会回 502。
- **凭据写入节流**：`host.auth.save` 会让宿主重载 auth 目录并回调 `auth.parse`，所以 `auth.parse` 里**不写**
  凭据，并对写入加了滑动窗口限流，避免自激循环。

## 已知限制（实测边界）

- **JWT 不可刷新**：上游没有 refresh 接口，30 天后必须重新登录/导出。插件会在控制台显示 api-key 状态，
  但无法延长 JWT 本身。
- **登录只实测到「请求被上游接受」**：本机没有第二个可登录账号（桌面端已登录、手机号打码、密码未知），
  因此手机号密码登录与短信登录都验证到上游返回语义错误（`2152 手机号或密码错误`、`2168 验证码错误`），
  没有完成一次真实的新账号登录。短信登录的图形验证码算法已复刻并实测通过 `POST /captcha/verify`。
- **图形验证码不校验图像内容**：实测只要 `angle` 字段按上游算法（`(52249517*(angle+nonce)+5976906165) mod 2^32`
  + 5 位 nonce）编码就能通过，与图片实际角度无关。这不是本插件的绕过，是上游校验强度如此。
- **积分回填是按「模型 + 时间窗」认领**：流水里没有 request id，无法一对一绑定；同一模型高频并发时可能
  出现个别请求认领不到（记 0）或相邻请求互相认领（总额仍正确）。
- **每日积分只在「个人组织」有意义**：上游 `login-gift` 对个人组织生效；团队组织返回未领取属正常。
- **账号健康度不落盘**：冷却、失败次数在进程内，重启清零；`dead`/`disabled`/优先级会落盘。
- **未实现 Responses / Anthropic 格式**：插件只声明 `chat-completions` 输入输出。
- **控制台需要 CPA 的管理密钥**：`/v0/management/*` 由宿主鉴权（`X-Management-Key`），
  浏览器直接打开会 401；正常入口是 CPA 控制台里的 **Magic2API** 菜单（GUI 自带鉴权）。
- **积分回填不占请求链路**：回填走单 worker + 有界队列（256），队列满时丢弃并记一条 warn，
  不影响对话本身。

## 许可

本仓库不含 LICENSE 文件；上游服务与客户端版权归广东灯塔引擎科技有限公司所有，本项目只做本机客户端协议的互通。
