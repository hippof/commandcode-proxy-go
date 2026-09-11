// Command embedproxy copies the proxy binary built by the repository root
// (../commandcode-proxy[.exe]) into proxybin/, so `wails3 build` embeds the
// current proxy into the desktop executable.
//
// It is wired into the build taskfiles but is also safe to run by hand. A
// missing source binary is not an error: the build then ships without an
// embedded proxy (the app reports that instead of failing to compile).
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

func main() {
	name := "commandcode-proxy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := filepath.Abs(filepath.Join("..", name))
	if err != nil {
		fmt.Println("embedproxy:", err)
		os.Exit(1)
	}
	dst, err := filepath.Abs(filepath.Join("proxybin", name))
	if err != nil {
		fmt.Println("embedproxy:", err)
		os.Exit(1)
	}

	in, err := os.Open(src)
	if err != nil {
		fmt.Printf("embedproxy: %s 不存在，跳过（本次构建不含内置代理）\n", src)
		return
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		fmt.Println("embedproxy:", err)
		os.Exit(1)
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		fmt.Println("embedproxy:", err)
		os.Exit(1)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		fmt.Println("embedproxy:", err)
		os.Exit(1)
	}
	if err := out.Close(); err != nil {
		fmt.Println("embedproxy:", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, dst); err != nil {
		fmt.Println("embedproxy:", err)
		os.Exit(1)
	}
	st, _ := os.Stat(dst)
	fmt.Printf("embedproxy: 已嵌入 %s（%d 字节）\n", dst, st.Size())
}
