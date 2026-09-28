package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func cloudflaredPath() (string, error) {
	if path := os.Getenv("CLOUDFLARED_PATH"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("CLOUDFLARED_PATH 不存在: %s", path)
	}
	path, err := exec.LookPath("cloudflared")
	if err != nil {
		return "", fmt.Errorf("未找到 cloudflared，请安装后设置 CLOUDFLARED_PATH（平台: %s/%s）: %w", runtime.GOOS, runtime.GOARCH, err)
	}
	return path, nil
}
