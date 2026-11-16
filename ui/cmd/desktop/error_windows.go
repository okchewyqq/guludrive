//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// A windowsgui executable may have no stderr to show startup failures. Use the
// OS dialog before/after MyGo runs, never raw errors that may contain a URL or
// credentials. Detailed safe command diagnostics remain available on stderr.
func showStartupError() {
	console := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	if handle, _, _ := console.Call(); handle != 0 {
		return
	}
	title, _ := windows.UTF16PtrFromString("GuluDrive")
	message, _ := windows.UTF16PtrFromString("GuluDrive 无法启动或未能安全关闭。\n请检查端口、配置及私有数据目录；首次使用请在界面创建管理员。请勿删除数据库或实例锁。\n\nGuluDrive could not start or shut down safely.\nCheck the port, configuration and private data directory. Create an administrator in the app on first use. Do not delete the database or instance lock.")
	const okError = 0x00000000 | 0x00000010 // MB_OK | MB_ICONERROR
	_, _, _ = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), okError)
}
