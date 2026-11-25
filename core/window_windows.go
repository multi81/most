//go:build windows

package core

// Одно окно «Моста»: окно — это Edge в режиме приложения с заголовком страницы «Мост». Щелчок по значку у часов
// (и повторный запуск exe) раньше каждый раз открывал новое окно;
// теперь уже открытое окно разворачивается и выводится вперёд.

import (
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetClassNameW       = user32.NewProc("GetClassNameW")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procIsIconic            = user32.NewProc("IsIconic")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop    = user32.NewProc("BringWindowToTop")
)

const windowTitle = "Мост" // <title> страницы web/index.html

// findWindow — видимое окно Edge/Chrome в режиме приложения с заголовком «Мост» (0 — нет).
// Обратный вызов для EnumWindows создаётся один раз: syscall.NewCallback не освобождается, а Go разрешает
// их не больше ~2000 — при создании на каждый поиск (ожидание окна опрашивает часто) программа со временем падала бы.
func findWindow() uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumFound = 0
	procEnumWindows.Call(enumCB, 0)
	return enumFound
}

var (
	enumMu    sync.Mutex
	enumFound uintptr
	enumCB    = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
			return 1
		}
		cls := make([]uint16, 64)
		procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&cls[0])), uintptr(len(cls)))
		if windows.UTF16ToString(cls) != "Chrome_WidgetWin_1" {
			return 1
		}
		t := make([]uint16, 128)
		procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&t[0])), uintptr(len(t)))
		if windows.UTF16ToString(t) == windowTitle {
			enumFound = hwnd
			return 0 // нашли — перебор окончен
		}
		return 1
	})
)

// focusWindow — если окно «Моста» уже открыто, развернуть его и вывести вперёд. true — было открыто.
func focusWindow() bool {
	h := findWindow()
	if h == 0 {
		return false
	}
	if ic, _, _ := procIsIconic.Call(h); ic != 0 {
		procShowWindow.Call(h, 9) // SW_RESTORE
	} else {
		procShowWindow.Call(h, 5) // SW_SHOW
	}
	procBringWindowToTop.Call(h)
	procSetForegroundWindow.Call(h)
	return true
}
