//go:build !windows

package main

// askYesNoImpl 非 Windows 平台直接返回 false（不阻塞无桌面环境）。
func askYesNoImpl(title, msg string) bool { return false }
