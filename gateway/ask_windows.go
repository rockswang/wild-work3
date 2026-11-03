//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var user32dll = windows.NewLazySystemDLL("user32.dll")

// askYesNoImpl 弹 Win32 是/否框（MB_YESNO|MB_ICONQUESTION + 置顶抢前台）。
//
// 不用 MB_DEFAULT_DESKTOP_ONLY：那是服务端语义，实测会把窗口甩到屏幕角落，
// 且在锁屏/RDP 下永不返回（旧版踩过，见 dev/internal/platform/platform_windows.go 注释）。
func askYesNoImpl(title, msg string) bool {
	proc := user32dll.NewProc("MessageBoxW")
	caption, _ := syscall.UTF16PtrFromString(title)
	text, _ := syscall.UTF16PtrFromString(msg)
	const mbYesNo, mbIconQuestion, mbTopmost, mbSetForeground = 0x4, 0x20, 0x40000, 0x10000
	res, _, _ := proc.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)),
		uintptr(mbYesNo|mbIconQuestion|mbTopmost|mbSetForeground))
	return res == 6 // IDYES
}
