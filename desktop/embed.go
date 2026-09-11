// Proxy binary embedding. The asset lives in proxybin/ next to this file (see
// proxybin/README.md); the build still succeeds without it, in which case the
// app reports that the proxy is missing instead of failing to compile.
package main

import (
	"embed"
	"io/fs"

	"commandcode-desktop/internal/proxy"
)

//go:embed all:proxybin
var proxyAssets embed.FS

func init() {
	// Either platform's file name may be present (a Windows checkout only
	// produces the .exe).
	for _, name := range []string{"proxybin/" + proxy.BinaryName(), "proxybin/commandcode-proxy.exe", "proxybin/commandcode-proxy"} {
		if b, err := fs.ReadFile(proxyAssets, name); err == nil && len(b) > 0 {
			proxy.SetEmbeddedBinary(b)
			return
		}
	}
}
