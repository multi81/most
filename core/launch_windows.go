//go:build windows

package core

// Запуск Chrome и Claude через «наш канал» (локальный прокси «Моста»), проверка адреса выхода, настройки программы.
// Chrome открывается с отдельным профилем (%APPDATA%\Most\chrome) — иначе флаг прокси игнорируется, если обычный
// Chrome уже открыт. Остальные программы и системный прокси (INCY и т.п.) не трогаются.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func findExe(cands ...string) string {
	for _, c := range cands {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func chromePath() string {
	rel := `Google\Chrome\Application\chrome.exe`
	return findExe(filepath.Join(os.Getenv("ProgramFiles"), rel), filepath.Join(os.Getenv("ProgramFiles(x86)"), rel),
		filepath.Join(os.Getenv("LOCALAPPDATA"), rel))
}

func claudePath() string {
	base := filepath.Join(os.Getenv("LOCALAPPDATA"), "AnthropicClaude")
	if p := findExe(filepath.Join(base, "claude.exe")); p != "" {
		return p
	}
	m, _ := filepath.Glob(filepath.Join(base, "app-*", "claude.exe"))
	if len(m) > 0 {
		return m[len(m)-1]
	}
	return findExe(filepath.Join(os.Getenv("LOCALAPPDATA"), `Programs\Claude\Claude.exe`))
}

var proxyFlag = "--proxy-server=socks5://127.0.0.1:" + strconv.Itoa(socksPort)

func launchChrome() error {
	p := chromePath()
	if p == "" {
		return errors.New("Chrome не найден на этом компьютере")
	}
	dir := filepath.Join(appDir(), "chrome")
	return exec.Command(p, proxyFlag, "--user-data-dir="+dir, "--no-first-run", "--no-default-browser-check").Start()
}

func claudeRunning() bool {
	out, _ := hidden(exec.Command("tasklist", "/FI", "IMAGENAME eq claude.exe", "/NH")).Output()
	return strings.Contains(strings.ToLower(string(out)), "claude.exe")
}

var (
	claudeOnce  sync.Once
	claudeAumid string // Claude из магазина (MSIX)
)

func claudeAppx() string {
	claudeOnce.Do(func() { claudeAumid = appxAUMID("Claude", "Claude") })
	return claudeAumid
}

func claudeAvailable() bool { return claudePath() != "" || claudeAppx() != "" }

func launchClaude() error {
	if claudeRunning() {
		return errors.New("Claude уже открыт — закройте его полностью (в том числе значок у часов) и нажмите снова, иначе прокси не применится")
	}
	if p := claudePath(); p != "" {
		return exec.Command(p, proxyFlag).Start()
	}
	if id := claudeAppx(); id != "" {
		return activateAppx(id, proxyFlag)
	}
	return errors.New("приложение Claude не найдено на этом компьютере")
}

