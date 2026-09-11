# proxybin/ — 待嵌入的代理二进制

桌面程序用 `go:embed` 把本目录里的 commandcode-proxy 二进制打进自己的 exe，
运行时释放到应用数据目录再启动，因此**发布物只有一个 exe**，目标机器不需要
Go 工具链，也不需要代理的源码仓库。

## 打包步骤

```sh
# 1) 仓库根：构建代理
make build                      # -> ./commandcode-proxy(.exe)

# 2) 通常不用手动拷：desktop 的 build 任务会自动从仓库根拷贝过来
#    （等价的等价手动操作：cp commandcode-proxy.exe desktop/proxybin/）

# 3) 构建桌面程序
cd desktop && wails3 build      # -> bin/desktop.exe（内含代理）
```

没有放二进制时仍可构建（得到一个不含代理的包，启动代理会提示重新打包），
`proxybin/*.exe` / `proxybin/commandcode-proxy` 已被 gitignore，不进版本库。
