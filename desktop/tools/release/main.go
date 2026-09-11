// Command release assembles the single-file release artifact: it copies the
// built executable into dist/ under a versioned name and writes a SHA-256
// sidecar so the file can be shared and verified.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	in := flag.String("in", filepath.Join("bin", "cmdc-desktop.exe"), "built executable")
	outDir := flag.String("out", "dist", "output directory")
	version := flag.String("version", "dev", "version stamped into the file name")
	name := flag.String("name", "cmdc-desktop", "artifact base name")
	flag.Parse()

	// The artifact extension follows the target executable, not the host.
	ext := filepath.Ext(*in)
	ver := strings.TrimSpace(*version)
	if ver == "" {
		ver = "dev"
	}
	if _, err := os.Stat(*in); err != nil {
		fmt.Fprintf(os.Stderr, "release: %s 不存在，请先构建：%v\n", *in, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	out := filepath.Join(*outDir, fmt.Sprintf("%s-%s-%s-%s%s", *name, ver, runtime.GOOS, runtime.GOARCH, ext))

	src, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	defer src.Close()
	dst, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		dst.Close()
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	if err := dst.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	st, _ := os.Stat(out)
	sum := hex.EncodeToString(h.Sum(nil))
	if err := os.WriteFile(out+".sha256", []byte(sum+"  "+filepath.Base(out)+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	fmt.Printf("单文件产物：%s\n  大小：%.2f MB\n  SHA-256：%s\n", out, float64(st.Size())/(1024*1024), sum)
}
