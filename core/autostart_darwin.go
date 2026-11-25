//go:build darwin

package core

// macOS: автозапуск — агент в ~/Library/LaunchAgents (система читает его при входе, launchctl не нужен).
// Агент открывает приложение с ключом --hidden: только значок в строке меню, без окна.

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
)

const launchAgentLabel = "ru.most.app"

func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

// bundlePath — папка Мост.app: ядро лежит в Мост.app/Contents/Resources/most-core
func bundlePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(exe)))
}

func autostartEnabled() bool {
	_, err := os.Stat(launchAgentPath())
	return err == nil
}

func setAutostart(on bool) error {
	p := launchAgentPath()
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b := bundlePath()
	if b == "" {
		return fmt.Errorf("не найдена папка приложения")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array>
    <string>/usr/bin/open</string><string>-g</string><string>%s</string><string>--args</string><string>--hidden</string>
  </array>
  <key>RunAtLoad</key><true/>
</dict></plist>
`, launchAgentLabel, html.EscapeString(b))
	return os.WriteFile(p, []byte(body), 0o644)
}

// InitMacAutostart — при первом запуске включить автозапуск (дальше решает переключатель в окне).
// Если приложение переехало (агент указывает на старую папку) — обновить путь.
func InitMacAutostart() {
	mark := filepath.Join(appDir(), "autostart-init")
	if _, err := os.Stat(mark); err != nil {
		if setAutostart(true) == nil {
			_ = os.WriteFile(mark, []byte("1"), 0o600)
		}
		return
	}
	if autostartEnabled() {
		_ = setAutostart(true)
	}
}
