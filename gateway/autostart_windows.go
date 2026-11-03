//go:build windows

package main

import (
	"os"
	"os/exec"
)

// Windows 开机自启：写 HKCU\...\Run（仅当前用户，无需管理员权限）。
// 旧版 `internal/platform` 亦走注册表；这里不引新依赖，直接调 reg.exe。
const (
	autostartKey  = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	autostartName = "wild-work3"
)

func autostartEnabled() bool {
	out, err := exec.Command("reg", "query", autostartKey, "/v", autostartName).Output()
	return err == nil && len(out) > 0
}

func autostartSet(on bool) error {
	if !on {
		_ = exec.Command("reg", "delete", autostartKey, "/v", autostartName, "/f").Run()
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// 值为带引号的 exe 路径 + serve；数据/插件目录默认取 exe 同级，保持与手动启动一致。
	return exec.Command("reg", "add", autostartKey, "/v", autostartName, "/t", "REG_SZ",
		"/d", `"`+exe+`" serve`, "/f").Run()
}
