// Command version prints the build version (git describe, else "dev"). It is
// used by the taskfiles so the version is compiled into the single-file build
// without depending on shell tricks being available.
package main

import (
	"fmt"
	"os/exec"
	"strings"
)

func main() {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		fmt.Print("dev")
		return
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		v = "dev"
	}
	fmt.Print(v)
}
