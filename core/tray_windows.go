//go:build windows

package core

// Значок у часов: щелчок — открыть окно; правая кнопка — меню (подключиться/отключиться, «через наш канал», выход).
// Окно «Моста» можно закрывать — программа продолжает работать в трее. Действия меню идут через тот же локальный
// API, что и окно (одна логика на всё).

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"fyne.io/systray"
)

//go:embed web/tray.ico
var trayIcon []byte

func (a *App) callAPI(path string, body any) (map[string]any, error) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	method := "GET"
	if body != nil {
		method = "POST"
	}
	req, _ := http.NewRequest(method, "http://"+uiAddr+path, rd)
	req.Header.Set("X-Token", a.token)
	cl := http.Client{Timeout: 60 * time.Second}
	r, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(r.Body).Decode(&out)
	if r.StatusCode != 200 {
		return out, fmt.Errorf("%v", out["error"])
	}
	return out, nil
}

func (a *App) openUI() { openWindow("http://" + uiAddr + "/?t=" + a.token) }

func (a *App) runTray() {
	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTooltip("Мост")
		// открытие окна может ждать его появления до нескольких секунд — не в обработчике значка, иначе значок
		// замирает и щелчки копятся
		systray.SetOnTapped(func() { go a.openUI() })
		mOpen := systray.AddMenuItem("Открыть «Мост»", "")
		systray.AddSeparator()
		mConn := systray.AddMenuItem("Подключиться", "Открыть рабочий стол сервера")
		mProxy := systray.AddMenuItemCheckbox("Через наш канал", "Chrome и Claude через сервер", false)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Выход", "Выключить программу")
		connected := false
		refresh := func() {
			st, err := a.callAPI("/api/state", nil)
			if err != nil || st["profile"] == nil {
				systray.SetTooltip("Мост — сервер не выбран")
				mConn.Disable()
				return
			}
			prof, _ := st["profile"].(map[string]any)
			connected, _ = st["connected"].(bool)
			live := 0
			if chs, ok := st["channels"].([]any); ok {
				for _, c := range chs {
					if m, _ := c.(map[string]any); m != nil && m["ok"] == true {
						live++
					}
				}
			}
			tip := fmt.Sprintf("Мост — %v: ", prof["name"])
			switch {
			case connected:
				tip += "подключено"
				mConn.SetTitle("Отключиться")
				mConn.Enable()
			case live > 0:
				tip += fmt.Sprintf("готово (каналов: %d)", live)
				mConn.SetTitle("Подключиться")
				if prof["rdp"] == true {
					mConn.Enable()
				} else {
					mConn.Disable()
				}
			default:
				tip += "проверяю каналы…"
				mConn.SetTitle("Подключиться")
				mConn.Disable()
			}
			systray.SetTooltip(tip)
			if px, ok := st["proxy"].(map[string]any); ok && px["on"] == true {
				mProxy.Check()
			} else {
				mProxy.Uncheck()
			}
		}
		go func() {
			for {
				refresh()
				time.Sleep(3 * time.Second)
			}
		}()
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					go a.openUI()
				case <-mConn.ClickedCh:
					if connected {
						_, _ = a.callAPI("/api/disconnect", map[string]string{})
					} else if _, err := a.callAPI("/api/connect", map[string]string{}); err != nil {
						go a.openUI() // ошибка видна в окне
					}
					refresh()
				case <-mProxy.ClickedCh:
					_, _ = a.callAPI("/api/proxy", map[string]bool{"On": !mProxy.Checked()})
					refresh()
				case <-mQuit.ClickedCh:
					_, _ = a.callAPI("/api/quit", map[string]string{})
				}
			}
		}()
	}, func() {})
}
