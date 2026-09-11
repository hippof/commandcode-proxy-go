# desktop/ — Command Code 账号管理器（纯托盘）

多账号**凭证**管理工具：把 `cmdc login` 登录得到的 `auth.json` 存进保管库、
随时切换。**没有主窗口、没有前端**——系统托盘菜单就是全部界面，结果用系统
原生弹窗反馈。

## 职责边界（刻意的设计）

| 做 | 不做 |
|---|---|
| 读取当前登录凭证（`~/.commandcode/auth.json`）并保存到保管库 | 不代替你登录（登录永远是终端里的 `cmdc login`） |
| 在多个已保存凭证之间切换（写回 auth.json + 导出 active.key） | 不改动上游 commandcode-proxy-go 仓库任何文件 |
| 探测每个账号的套餐额度（走本地代理） | 不联网上传任何凭据 |
| 管理本地代理进程（启动/停止/仪表盘） | 不从源码构建代理（二进制随桌面程序一起打包） |
| 本地网关：把静态 key 客户端的请求换成当前账号（让所有 IDE 跟随托盘切换） | 不接收外部连接（只监听回环地址） |

登录交给 CLI 本身是有意为之：它的登录是交互式 TUI（且已登录时会直接退出、
不弹浏览器），由程序代跑既脆弱又容易误覆盖正在使用的账号。

## 与上游仓库的关系（fork 安全）

- 本目录是**独立 Go module**（`commandcode-desktop`），不 import 主模块的
  `internal/`，也不往主 `go.mod` 加依赖。
- 全仓库对上游跟踪文件的改动为零；合并上游更新时本目录不参与冲突。

## 运行

双击 `bin/desktop.exe` 即常驻托盘（无窗口，内存约 20MB）。

菜单**单层横铺**，用分隔线分组；**当前用不上的项直接置灰、点了不会报错**：

```
· 当前：kuqinqinmqxn（user_4Nt…nyFS）   ← 状态行，不可点
──────────────────────────────────────
· ● kuqinqinmqxn — 无额度               ← 当前所用，仅作状态标记
  ○ 备用号 — 可用模型 3                  ← 点击即切换
──────────────────────────────────────
  保存当前登录凭证                       ← 无 auth.json 时置灰
  停用当前登录                           ← 未登录时置灰
  刷新套餐额度                           ← 保管库为空 / 代理无法拉起时置灰
──────────────────────────────────────
  删除保管库副本：备用号                  ← 只列出可安全删除的（在用账号不出现）
──────────────────────────────────────
  ☑ 本地代理：运行中（http://127.0.0.1:8787）
  打开仪表盘 /admin                      ← 代理未运行时置灰
──────────────────────────────────────
  ☑ IDE 网关：运行中（http://127.0.0.1:54321）
  复制网关接入信息                        ← 复制粘贴用的接入片段
──────────────────────────────────────
  打开日志目录                           ← 打开按天分文件的日志目录
  打开保管库目录
  退出
```

外部代理占用端口时，代理那行会显示"外部实例运行中"并置灰（既不归我们启动也不归我们停止）；
账号切换、删除条目、置灰状态都会在每次操作后立即重算。

## 典型工作流

1. **登录（手动）**：终端里 `cmdc login`（macOS/Linux：`cmd login`），浏览器授权。
2. **保存**：托盘 →「保存当前登录凭证」→ 存入保管库（弹窗确认账号名）。
3. **重复** 1–2 收集多个账号；托盘 →「刷新套餐额度」看每个账号的额度状态。
4. **切换**：托盘里直接点目标账号那一行（当前账号那行是状态标记，点不动）。切换同时写入
   `~/.commandcode/auth.json` 与 `%APPDATA%\commandcode-desktop\active.key`，
   CLI 下次启动、消费端下次请求即用新账号（代理无需重启，它是 keyless 的）。

## 数据位置

| 内容 | 路径 |
|---|---|
| 保管库（auth.json 副本 + 备注 + 套餐缓存） | `~/.commandcode-accounts/accounts/<userId>/` |
| 应用配置 / active.key / keyhelper.cmd / proxy.log | `%APPDATA%\commandcode-desktop\` |
| 从内置资源释放出来的代理二进制（自动，无需手工） | `%APPDATA%\commandcode-desktop\commandcode-proxy.exe` |
| CLI 凭据目录（读写的目标） | `%USERPROFILE%\.commandcode\auth.json` |

## 接入 Claude Code（切换即生效）

```json
{
  "env": { "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787" },
  "apiKeyHelper": "C:\\Users\\<you>\\AppData\\Roaming\\commandcode-desktop\\keyhelper.cmd"
}
```

Claude Code 会缓存 apiKeyHelper 的返回值；切换后可重启它，或设
`CLAUDE_CODE_API_KEY_HELPER_TTL_MS=60000` 让它每分钟刷新。

## 给 IDE / 编辑器接入（本地网关，推荐）

代理本身是 keyless 的：它按请求头里的 key 转发，自己不认识账号。多数编辑器只能
在 provider 里填一个**静态 key**，于是"托盘切换"对它们无效。**本地网关**解决这件事：

- 网关监听**固定端口 54321**（刻意选的不常用端口，编辑器记住它就不该漂移），把请求转发给本地代理，并把客户端送来的
  key **替换成托盘当前激活账号的 key**（每次请求实时读取，无缓存）；
- 编辑器里 key 随便填占位符（如 `sk-local`），**之后托盘一换，编辑器下一个请求就是新账号**；
- 代理没在跑时网关会**自动拉起**它，所以只要托盘应用在运行，编辑器就一直可用；
- 除 key 之外完全透明：路径、查询串、SSE 流式响应原样透传（含 `/v1/messages/count_tokens`、`/health`、`/v1/models`）。

两个地址（配合编辑器支持哪套协议）：

| 协议 | baseURL |
|---|---|
| Anthropic Messages | `http://127.0.0.1:54321` |
| OpenAI 兼容 | `http://127.0.0.1:54321/v1` |

模型 ID 填 Command Code 的真实 ID（例：`deepseek/deepseek-v4-flash`、`deepseek/deepseek-v4-pro`、
`moonshotai/kimi-k3`、`zai-org/glm-5.2`）；也可填 `claude-sonnet-5` 这类，代理会做家族映射。
托盘 →「复制网关接入信息」可直接把上面这些粘到剪贴板。

### 各编辑器怎么填

| 软件 | 选哪套协议 | 填法 |
|---|---|---|
| **ZCode** | Anthropic | 设置里添加自定义 provider：baseURL `http://127.0.0.1:54321`，kind=anthropic，key 填 `sk-local`，模型填 CC 模型 ID |
| **Cursor** | OpenAI | Settings → Models：OpenAI Key 填 `sk-local`，勾选 Override OpenAI Base URL 填 `http://127.0.0.1:54321/v1`，再 + Add Model |
| **Claude Code** | Anthropic | `ANTHROPIC_BASE_URL=http://127.0.0.1:54321` + 任意 key（比 apiKeyHelper 更简单，也不用管缓存） |
| **Cline / Roo / Kilo** | OpenAI 兼容 | Provider 选 OpenAI Compatible，Base URL `http://127.0.0.1:54321/v1` |
| **Continue** | 两者皆可 | `apiBase` 填 `http://127.0.0.1:54321/v1`（openai）或 `http://127.0.0.1:54321`（anthropic） |
| **opencode** | OpenAI 兼容 | provider baseURL `http://127.0.0.1:54321/v1` |

### 启动顺序与端口

托盘里两个开关（本地代理、IDE 网关）都支持**启停**并**记住选择**（`proxyAuto` / `gatewayAuto`），
启动时严格按顺序执行，保证网关不会指向一个不存在的端口：

1. **先起本地代理**：默认 8787；**若该端口被占用，自动换一个空闲端口启动**（日志记 WARN，
   托盘标签显示实际地址）；
2. **网关拿到代理的实际地址后**才启动：网关**每次请求都向代理重新取地址**，所以代理换端口
   （或你手动停掉后又被自动拉起）都不会打错；
3. 若代理起不来，网关**不会启动**，而是弹错误通知并写日志——不会出现"网关在跑但连不上代理"。

网关端口是**固定的 54321**（`gatewayPort`，改动需重启；旧版本默认 8790 的配置会自动迁移）。
托盘标签悬停可看到它当前指向的代理地址。

## 开发 / 构建

**发布物是单个 exe**：代理二进制用 `go:embed` 打进桌面程序，运行时释放到应用
数据目录再启动，所以目标机器**不需要 Go 工具链，也不需要代理源码**。

```sh
# 1) 仓库根：构建代理（上游更新合并后重新走这一步）
go build -o commandcode-proxy.exe ./cmd/commandcode-proxy   # 或 make build

# 2) 构建桌面程序：build 任务会自动把上面那个二进制拷进 proxybin/ 再嵌入
cd desktop && wails3 build      # -> bin/desktop.exe（内含代理，约 19MB）

go test ./...   # 保管库 / 凭据目录 / 代理释放 / 探测 / 托盘动作 均有单测
```

没构建代理也能 `wails3 build`（得到一个不含代理的包，启动代理时会明确提示），
`proxybin/commandcode-proxy*` 已 gitignore，不进版本库。

## 日志

所有环节的错误都会记录到**按天分文件**的日志里（即使通知气泡被你随手关掉也能追溯）：

- 位置：`%APPDATA%\commandcode-desktop\logs\2026-09-11.log`（文件名即当天日期，
  跨天自动新建文件，无需重启；托盘 →「打开日志目录」直达）；
- 覆盖：启动/自检、内置代理释放与子进程启停、网关启停与请求期错误（上游不可达、
  没有激活账号——同一类错误最多每分钟记一次以免刷屏）、账号保存/切换/停用/删除、
  套餐探测逐账号结果，以及**所有通知**（所有用户可见的失败都经 `notifier` 出口，
  在那层统一记录，保证不漏）；
- 行格式：`2026-09-11 15:58:52.099 [INFO] gateway: 开始监听 addr=… upstream=…`
  （`[DEBUG] [INFO] [WARN] [ERROR]`；多行消息会转义成单行，一条一行）；
- 代理子进程自身的输出：`logs/commandcode-proxy.log`（代理自己写结构化日志，不按天切）；
- 配置：`logLevel`（debug/info/warn/error，默认 info）与 `logKeepDays`（保留天数，默认 14，
  启动时清理超期文件）；
- **密钥永不入日志**：只记录账号名、userId 与掩码后的 key（有测试守着这条不变量）。

## 打包（单文件软件）

发布物就是**一个 exe**：桌面程序 + 内嵌的代理二进制，目标机器不需要 Go 工具链、
不需要代理源码、不需要前端资源。一条命令产出：

```sh
cd desktop
wails3 task release:single
# -> dist/commandcode-desktop-<版本>-windows-amd64.exe（约 20MB）
#    以及同名 .sha256 校验文件
```

版本号来自 `git describe --tags --always --dirty`（工具 `tools/version`），编译进
二进制并在启动日志里输出（`app: 启动 version=…`）；要指定版本可手动跑
`go run ./tools/release -version v1.0.0`。

单文件运行时唯一写入的位置（都是用户数据，随程序走）：

| 内容 | 位置 |
|---|---|
| 账号保管库 | `%USERPROFILE%\.commandcode-accounts\` |
| 配置 / 日志 / 导出的 key / 释放出来的代理 | `%APPDATA%\commandcode-desktop\` |
| CLI 凭据目录（读写目标） | `%USERPROFILE%\.commandcodeuth.json` |

已验证：把该 exe 单独放进一个空目录、并删掉 `%APPDATA%` 里的代理副本后运行，
它会自动释放内置代理、按序启动代理与网关，经网关发出的请求能得到真实模型回复。

## 安全

- API key 只存在于本机保管库与 CLI 凭据目录；界面与弹窗只显示掩码。
- 不写入仓库、不打日志；程序不发起除 Command Code API 之外的任何网络请求。

## 历史说明

早期版本曾尝试代替用户完成登录（临时挪走凭据目录 + 拉起 CLI 向导），该逻辑
已移除。启动时保留一次兼容性恢复：若发现遗留的交换临时目录且当前没有有效
凭证，就把它放回去（保护从旧版本升级的用户）。
