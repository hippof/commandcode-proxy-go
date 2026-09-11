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

## 安全

- API key 只存在于本机保管库与 CLI 凭据目录；界面与弹窗只显示掩码。
- 不写入仓库、不打日志；程序不发起除 Command Code API 之外的任何网络请求。

## 历史说明

早期版本曾尝试代替用户完成登录（临时挪走凭据目录 + 拉起 CLI 向导），该逻辑
已移除。启动时保留一次兼容性恢复：若发现遗留的交换临时目录且当前没有有效
凭证，就把它放回去（保护从旧版本升级的用户）。
