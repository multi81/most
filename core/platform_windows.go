//go:build windows

package core

// Windows: каталоги в профиле пользователя, профили шифруются DPAPI, выход убирает значок из трея.

import (
	"os"
	"os/exec"
	"path/filepath"

	"fyne.io/systray"
)

type rdpProc interface {
	Wait() error
	Kill() error
}

func appDir() string {
	d := filepath.Join(os.Getenv("APPDATA"), "Most")
	_ = os.MkdirAll(d, 0o700)
	return d
}

func binDir() string {
	d := filepath.Join(os.Getenv("LOCALAPPDATA"), "Most", "bin")
	_ = os.MkdirAll(d, 0o700)
	return d
}

func sealSecret(b []byte) ([]byte, error) { return dpapiEncrypt(b) }
func openSecret(b []byte) ([]byte, error) { return dpapiDecrypt(b) }

func platformQuit()            { systray.Quit() }
func updatesSupported() bool   { return true }
func startExe(f string) error  { return exec.Command(f).Start() }
func rdpHint(p *Profile) string { return "" } // на Windows окно RDP открывается само
