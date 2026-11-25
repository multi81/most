//go:build darwin

// most-core — ядро «Моста» для macOS: запускается оболочкой (Most.app, Swift) и печатает в первую строку адрес
// окна; оболочка показывает его в WKWebView. Данные — ~/Library/Application Support/Most.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"most/core"
)

func main() {
	home, _ := os.UserHomeDir()
	core.SetDataDir(filepath.Join(home, "Library", "Application Support", "Most"))
	core.SetOpenURL(func(u string) error { return exec.Command("open", u).Run() })
	core.InitMacAutostart()
	url, err := core.StartEmbedded()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка запуска:", err)
		os.Exit(1)
	}
	fmt.Println(url)
	select {}
}
