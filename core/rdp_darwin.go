//go:build darwin

package core

// macOS: рабочий стол открывается в Microsoft «Windows App» (App Store) — файлом .rdp на локальный вход «Моста».
// Пароль приложение спросит один раз и сохранит в Связке ключей (окно «Моста» подскажет и скопирует его).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Папка обмена: ~/Обмен-сервер. Windows App на маке передаёт в сеанс папку, выбранную в его настройках
// («Основные» → перенаправление папок для файлов RDP), если в .rdp включено drivestoredirect; на сервере она
// видна на рабочем столе как «Обмен» (xrdp монтирует её в ~/thinclient_drives).
// Имя латиницей: RDP обрезает имя папки до 8 байт («Обмен-сервер» на сервере было «Обме»).
const exchangeName = "Obmen"

func exchangeDir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, exchangeName)
	// прежняя папка (до 0.9.2) переименовывается вместе с файлами
	if _, err := os.Stat(d); os.IsNotExist(err) {
		_ = os.Rename(filepath.Join(home, "Обмен-сервер"), d)
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

func openFolder(dir string) { _ = exec.Command("open", dir).Start() }

func launchRDP(p *Profile, port int) (rdpProc, error) {
	exchangeDir()
	body := fmt.Sprintf("full address:s:127.0.0.1:%d\nusername:s:%s\nprompt for credentials:i:0\nautoreconnection enabled:i:1\n"+
		"screen mode id:i:2\ndynamic resolution:i:1\nsession bpp:i:32\naudiomode:i:0\nredirectclipboard:i:1\n"+
		"drivestoredirect:s:*\n", port, p.RDPUser)
	path := filepath.Join(appDir(), "server.rdp")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return nil, err
	}
	// именно в Windows App (-b): «чем откроется» у файла .rdp бывает другой клиент,
	// например «Remote Desktop Connection» из Parallels
	if err := exec.Command("open", "-b", "com.microsoft.rdc.macos", path).Run(); err != nil {
		return nil, errors.New("не открылся RDP-клиент — установите Microsoft «Windows App» из App Store")
	}
	return nil, nil
}
