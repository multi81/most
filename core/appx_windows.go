//go:build windows

package core

// Запуск приложения из магазина (MSIX, например Claude) с параметрами командной строки — через системный
// ApplicationActivationManager (COM). По пути внутри WindowsApps такие приложения с параметрами не запускаются.

import (
	"fmt"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	clsidAAM             = windows.GUID{Data1: 0x45BA127D, Data2: 0x10A8, Data3: 0x46EA, Data4: [8]byte{0x8A, 0xB7, 0x56, 0xEA, 0x90, 0x78, 0x94, 0x3C}}
	iidAAM               = windows.GUID{Data1: 0x2e941141, Data2: 0x7f97, Data3: 0x4756, Data4: [8]byte{0xba, 0x1d, 0x9d, 0xec, 0xde, 0x89, 0x4a, 0x3d}}
)

// appxAUMID — идентификатор запуска пакета (PackageFamilyName!AppId) по имени пакета, или "".
func appxAUMID(pkg, appID string) string {
	out, err := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-AppxPackage -Name '"+pkg+"' | Select-Object -First 1).PackageFamilyName")).Output()
	pfn := strings.TrimSpace(string(out))
	if err != nil || pfn == "" {
		return ""
	}
	return pfn + "!" + appID
}

func activateAppx(aumid, args string) error {
	runtime.LockOSThread() // COM привязан к потоку
	defer runtime.UnlockOSThread()
	procCoInitializeEx.Call(0, 0x2) // COINIT_APARTMENTTHREADED; повторный вызов безвреден
	type comObj struct{ vtbl *[6]uintptr } // IUnknown(3) + ActivateApplication, ActivateForFile, ActivateForProtocol
	var obj *comObj
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidAAM)), 0, 0x4 /*CLSCTX_LOCAL_SERVER*/, uintptr(unsafe.Pointer(&iidAAM)), uintptr(unsafe.Pointer(&obj)))
	if hr != 0 || obj == nil {
		return errors.New("не удалось обратиться к запуску приложений Windows")
	}
	self := uintptr(unsafe.Pointer(obj))
	defer syscall.SyscallN(obj.vtbl[2], self) // Release
	a, _ := windows.UTF16PtrFromString(aumid)
	g, _ := windows.UTF16PtrFromString(args)
	var pid uint32
	hr, _, _ = syscall.SyscallN(obj.vtbl[3], self, uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&pid)))
	if hr != 0 {
		return fmt.Errorf("Windows не запустил приложение (код 0x%08X)", uint32(hr))
	}
	return nil
}
