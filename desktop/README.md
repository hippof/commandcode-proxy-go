# desktop/ — Command Code 账号管理器（Wails v3）

多账号管理桌面应用：保管 `auth.json` 账号库、浏览器登录新账号、一键切换，并
管理本地 `commandcode-proxy`。

## 与上游仓库的关系（fork 安全）

- 本目录是**独立 Go module**（`commandcode-desktop`），不 import 主模块的
  `internal/`，也不往主 `go.mod` 加任何依赖。
- 全仓库对上游跟踪文件的改动为零；上游更新合并时本目录不会参与冲突。
- 代理以**子进程**方式使用：优先复用仓库根已构建的 `commandcode-proxy.exe`，
  没有就调用 `go build ./cmd/commandcode-proxy` 构建到
  `%APPDATA%\commandcode-desktop\` 下，绝不重新打包代理源码。

## 开发 / 构建

```sh
# 依赖：Go 1.25+、Node（vite）、wails3 CLI
#   go install github.com/wailsapp/wails/v3/cmd/wails3@latest
wails3 dev      # 热重载开发
wails3 build    # -> bin/desktop.exe
go test ./...   # 单测
```

## 数据位置

| 内容 | 路径 |
|---|---|
| 账号库（auth.json 副本） | `~/.commandcode-accounts/accounts/<userId>/`（可在配置里改） |
| 应用配置 / active.key / keyhelper.cmd / proxy.log | `%APPDATA%\commandcode-desktop\` |
| CLI 凭据目录（切换目标） | `%USERPROFILE%\.commandcode\auth.json` |

## 工作原理

- **切换账号** = 把账号库里的 auth.json 原子写回 CLI 凭据目录 + 更新
  `active.key`（消费端用）+ 记录 active 标记。
- **登录新账号** = 整个凭据目录临时挪开（`.commandcode.ccdesktop-tmp`）→
  子进程跑 `commandcode login`（弹出一个**可见的终端向导窗口**——CLI 的登录
  界面是需要按键选择的 TUI，在其中选择登录方式后会再拉起浏览器授权）→
  轮询新 auth.json → 存入账号库
  → 原目录放回。**当前账号完全不受影响**，确认后手动点「切换」才激活。应用
  崩溃后重启会自动放回被挪开的目录（RecoverAfterRestart）。
- **套餐探测**：用账号库里每个账号的 key（代理是 keyless 的，key 走请求头，
  所以**不切换账号也能查任意账号**）通过本地代理发最小请求：200/429=可用，
  402/403=不在套餐。结果缓存于账号目录 `plan.json`（6 小时过期）。

## 接入 Claude Code（切换即生效，无需重启）

settings.json 中：

```json
{
  "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
  "apiKeyHelper": "C:\\Users\\<you>\\AppData\\Roaming\\commandcode-desktop\\keyhelper.cmd"
}
```

代理端点同时支持 OpenAI（`/v1/chat/completions`）与 Anthropic（`/v1/messages`）
兼容协议；`/admin` 有请求日志仪表盘。

## 托盘 / 窗口关闭

- 系统托盘常驻：左键单击显示主界面；右键菜单 = 打开主界面 / **切换账号**（单选列表）/
  本地代理开关 / 仪表盘 / 关闭行为 / 退出程序。
- 点窗口 × 的行为可在主界面下拉框设置：默认**最小化到托盘**（代理子进程继续运行），
  改为"退出程序"则真正退出（会停掉由本程序管理的代理）。

## 安全

- API key 仅存于账号库与 CLI 凭据目录（本机），UI 只显示掩码；不写入仓库、
  不打日志。
