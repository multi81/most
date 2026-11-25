//go:build windows

package core

// Запуск mstsc: пароль кладётся в диспетчер учётных данных, файл .rdp пишется в %APPDATA%\Most.
// Папка обмена: %USERPROFILE%\Обмен-сервер подключается буквой диска (subst) и передаётся в сеанс RDP —
// на сервере она видна на рабочем столе как «Обмен» (xrdp монтирует её в ~/thinclient_drives).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const exchangeName = "Обмен-сервер"

func exchangeDir() string {
	d := filepath.Join(os.Getenv("USERPROFILE"), exchangeName)
	_ = os.MkdirAll(d, 0o755)
	return d
}

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 /*CREATE_NO_WINDOW*/}
	return cmd
}

// exchangeDrive — буква диска для папки обмена: уже назначенная ранее или первая свободная с конца алфавита.
func exchangeDrive() string {
	dir := exchangeDir()
	out, _ := hidden(exec.Command("subst")).Output()
	for _, line := range strings.Split(string(out), "\n") { // «O:\: => C:\Users\...\Обмен-сервер»
		if l, r, ok := strings.Cut(strings.TrimSpace(line), ": => "); ok && strings.EqualFold(strings.TrimSpace(r), dir) {
			return strings.TrimSuffix(l, `\`)
		}
	}
	for c := 'S'; c >= 'M'; c-- {
		d := string(c) + ":"
		if _, err := os.Stat(d + `\`); os.IsNotExist(err) {
			if hidden(exec.Command("subst", d, dir)).Run() == nil {
				return d
			}
		}
	}
	return ""
}

func writeRDPFile(port int, user string) (string, error) {
	drive := exchangeDrive()
	redirect := ""
	if drive != "" {
		redirect = drive + `\;`
	}
	body := fmt.Sprintf(`full address:s:127.0.0.1:%d
username:s:%s
prompt for credentials:i:0
promptcredentialonce:i:0
authentication level:i:0
enablecredsspsupport:i:0
negotiate security layer:i:1
autoreconnection enabled:i:1
autoreconnect max retries:i:100
screen mode id:i:2
use multimon:i:0
smart sizing:i:1
dynamic resolution:i:1
session bpp:i:32
compression:i:1
connection type:i:6
networkautodetect:i:0
bandwidthautodetect:i:1
displayconnectionbar:i:1
redirectclipboard:i:1
redirectprinters:i:0
redirectsmartcards:i:0
audiomode:i:0
keyboardhook:i:2
drivestoredirect:s:%s
`, port, user, redirect)
	path := filepath.Join(appDir(), "server.rdp")
	return path, os.WriteFile(path, []byte(body), 0o600)
}

func launchRDP(p *Profile, port int) (rdpProc, error) {
	if err := saveRDPCredential("127.0.0.1", p.RDPUser, p.RDPPassword); err != nil {
		return nil, fmt.Errorf("пароль не сохранился в Windows: %w", err)
	}
	path, err := writeRDPFile(port, p.RDPUser)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("mstsc", path)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmdProc{cmd}, nil
}

// cmdProc — окно mstsc как rdpProc.
type cmdProc struct{ c *exec.Cmd }

func (p cmdProc) Wait() error { return p.c.Wait() }
func (p cmdProc) Kill() error { return p.c.Process.Kill() }

func openFolder(dir string) { _ = exec.Command("explorer", dir).Start() }

func openLink(u string) { _ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start() }
