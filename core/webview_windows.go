//go:build windows

package core

// Встроенное окно «Моста» (WebView2 — движок Edge, входит в Windows 10/11; библиотека go-webview2, MIT, без CGO).
// Как на маке: окно одно на всё время работы — закрытие крестиком только прячет его, щелчок по значку показывает
// (раньше окно было отдельным процессом Edge, отсюда копии,
// поиск чужих окон и ожидание). Создаётся при первом открытии (тихий автозапуск не держит движок в памяти).
// Нет WebView2 в системе — openWindow откроет окно по-старому, через Edge.

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

var (
	procSetWindowLongPtrW = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW   = user32.NewProc("CallWindowProcW")
	procSendMessageW      = user32.NewProc("SendMessageW")
	procExtractIconW      = windows.NewLazySystemDLL("shell32.dll").NewProc("ExtractIconW")

	wvMu       sync.Mutex
	wvHwnd     uintptr       // окно (0 — ещё не создано или WebView2 недоступен)
	wvReady    chan struct{} // идёт создание окна
	wvFailed   bool          // WebView2 не создался — дальше сразу старый способ
	wvOrigProc uintptr       // оконная процедура библиотеки (наша подменяет только закрытие)

	// создаётся один раз (syscall.NewCallback не освобождается)
	wvProc = syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		if msg == 0x0010 { // WM_CLOSE — спрятать, а не уничтожать
			procShowWindow.Call(hwnd, 0) // SW_HIDE
			return 0
		}
		r, _, _ := procCallWindowProcW.Call(wvOrigProc, hwnd, msg, wp, lp)
		return r
	})
)

const gwlpWndproc = ^uintptr(3) // GWLP_WNDPROC = -4

// showNative — показать встроенное окно (при первом вызове — создать). false — WebView2 недоступен.
func showNative(url string) bool {
	wvMu.Lock()
	if wvFailed {
		wvMu.Unlock()
		return false
	}
	if wvHwnd != 0 {
		h := wvHwnd
		wvMu.Unlock()
		raise(h)
		return true
	}
	if ch := wvReady; ch != nil { // окно как раз создаётся (быстрые щелчки) — дождаться его
		wvMu.Unlock()
		<-ch
		return showNative(url)
	}
	ch := make(chan struct{})
	wvReady = ch
	wvMu.Unlock()

	go func() {
		runtime.LockOSThread() // у окна свой поток со своим циклом сообщений (значок у часов — в главном)
		w := webview2.NewWithOptions(webview2.WebViewOptions{
			DataPath:  filepath.Join(os.Getenv("LOCALAPPDATA"), "Most", "WebView2"),
			AutoFocus: true,
			WindowOptions: webview2.WindowOptions{Title: "Мост", Width: 1040, Height: 740, Center: true},
		})
		wvMu.Lock()
		if w == nil {
			wvFailed, wvReady = true, nil
			wvMu.Unlock()
			close(ch)
			return
		}
		h := uintptr(w.Window())
		wvOrigProc, _, _ = procSetWindowLongPtrW.Call(h, gwlpWndproc, wvProc)
		wvHwnd, wvReady = h, nil
		wvMu.Unlock()
		setWindowIcon(h)
		w.SetSize(420, 560, webview2.HintMin)
		w.Navigate(url)
		close(ch)
		raise(h)
		w.Run() // цикл сообщений окна; окно не уничтожается (закрытие прячет)
	}()
	<-ch
	return showNative(url)
}

// raise — развернуть (если свёрнуто) и вывести вперёд.
func raise(h uintptr) {
	if ic, _, _ := procIsIconic.Call(h); ic != 0 {
		procShowWindow.Call(h, 9) // SW_RESTORE
	} else {
		procShowWindow.Call(h, 5) // SW_SHOW
	}
	procBringWindowToTop.Call(h)
	procSetForegroundWindow.Call(h)
}

// setWindowIcon — значок «Моста» из его exe (в заголовке и в панели задач), а не стандартный.
func setWindowIcon(h uintptr) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	p, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return
	}
	var mod windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &mod)
	icon, _, _ := procExtractIconW.Call(uintptr(mod), uintptr(unsafe.Pointer(p)), 0)
	if icon > 1 {
		procSendMessageW.Call(h, 0x0080, 1, icon) // WM_SETICON, ICON_BIG
		procSendMessageW.Call(h, 0x0080, 0, icon) // ICON_SMALL
	}
}
