//go:build !windows

package main

// 非 Windows 平台暂不实现开机自启（第一阶段优先 Windows，AGENTS §H）。
func autostartEnabled() bool     { return false }
func autostartSet(on bool) error { return nil }
