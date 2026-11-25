package core

// «Мост» — подключение к удалённому рабочему столу через несколько каналов с автовыбором (2026-11-15, Вера).
// Окно программы — локальная страница 127.0.0.1:47811 в окне Edge (режим приложения). Повторный запуск exe
// просто открывает окно уже работающей программы. Журнал: %APPDATA%\Most\most.log.

import (
	"context"
	"fmt"
	"embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

)

//go:embed web/index.html web/icon.svg
var webFS embed.FS

const uiAddr = "127.0.0.1:47811"
const rdpPort = 33400

var version = "0.9.26"

func itoa(i int) string { return strconv.Itoa(i) }

type App struct {
	mu      sync.Mutex
	profile *Profile
	fwd     *Forwarder
	token   string
	started time.Time
	mstsc   rdpProc     // окно RDP, открытое кнопкой «Подключиться» (на Android — nil)
	rdpOpen atomic.Bool // окно RDP ещё открыто
	egress  EgressProxy // «через наш канал»
	setup   Setup       // мастер подготовки сервера
	upd     Updater     // автообновление
	url     string      // адрес окна (встроенный режим)
	userDisc atomic.Bool  // нажато «Отключиться» — окно RDP не открывать заново
	reopens  []time.Time  // когда окно RDP открывалось заново после обрыва (предел 3 за 10 мин)
}


func (a *App) use(p *Profile) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.profile = p
	if a.fwd == nil {
		a.fwd = newForwarder(buildChannels(p), rdpPort)
		if err := a.fwd.listen(); err != nil {
			log.Printf("порт %d занят: %v", rdpPort, err)
		}
		go a.fwd.probeLoop()
		if loadSettings().Proxy {
			if err := a.egress.Start(a.fwd); err != nil {
				log.Printf("прокси не включился: %v", err)
			}
		}
	}
}

// openRDP — открыть окно рабочего стола и следить за ним: если окно закрылось само вскоре после обрыва канала
// (а не кнопкой «Отключиться»), открыть его снова — не больше 3 раз за 10 минут.
func (a *App) openRDP(p *Profile) error {
	cmd, err := launchRDP(p, rdpPort)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.mstsc = cmd
	a.mu.Unlock()
	if cmd == nil { // на Android окно RDP — чужое приложение, его процесс не отслеживаем
		return nil
	}
	a.rdpOpen.Store(true)
	go func() {
		_ = cmd.Wait()
		a.rdpOpen.Store(false)
		a.mu.Lock()
		f, cur := a.fwd, a.profile
		a.mu.Unlock()
		if a.userDisc.Load() || f == nil || cur != p || !f.LastDropWithin(3*time.Minute) {
			return // закрыли сами, сменили сервер или обрыва не было
		}
		a.mu.Lock()
		now := time.Now()
		var recent []time.Time
		for _, t := range a.reopens {
			if now.Sub(t) < 10*time.Minute {
				recent = append(recent, t)
			}
		}
		a.reopens = recent
		allow := len(recent) < 3
		if allow {
			a.reopens = append(a.reopens, now)
		}
		a.mu.Unlock()
		if !allow {
			log.Printf("окно RDP закрылось после обрыва, но автооткрытие уже было 3 раза за 10 минут — жду вас")
			return
		}
		log.Printf("окно RDP закрылось после обрыва — открываю снова через живой канал")
		time.Sleep(2 * time.Second)
		if err := a.openRDP(p); err != nil {
			log.Printf("автооткрытие не удалось: %v", err)
		}
	}()
	return nil
}

// switchTo — сменить активный сервер: старые каналы и прокси выключаются полностью, новые поднимаются.
func (a *App) switchTo(p *Profile) {
	a.mu.Lock()
	old := a.fwd
	a.fwd = nil
	a.mu.Unlock()
	if old != nil {
		a.egress.Stop()
		old.stop()
	}
	a.use(p)
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("/icon.svg", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/icon.svg")
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.URL.Query().Get("t") != a.token {
			http.Error(w, "нет доступа", 403)
			return
		}
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(strings.Replace(string(b), "__TOKEN__", a.token, 1)))
	})
	api := func(path string, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Token") != a.token { // защита от чужих страниц в браузере
				http.Error(w, "нет доступа", 403)
				return
			}
			h(w, r)
		})
	}
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		w.WriteHeader(400)
		reply(w, map[string]string{"error": err.Error()})
	}

	api("/api/state", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		st := map[string]any{"version": version, "profile": nil, "exchange": exchangeDir(), "pid": os.Getpid(), "platform": runtime.GOOS,
			"autostart": autostartEnabled(), "uptime": int(time.Since(a.started).Seconds())}
		if a.profile != nil {
			st["profile"] = map[string]any{"name": a.profile.Name, "server": a.profile.Server, "user": a.profile.RDPUser,
				"admin": a.profile.AdminToken != "", "rdp": a.profile.RDPUser != ""}
			st["channels"] = a.fwd.snapshot()
			a.fwd.mu.Lock()
			st["mode"] = a.fwd.mode
			a.fwd.mu.Unlock()
			st["connected"] = a.fwd.activeCount() > 0 || a.rdpOpen.Load()
			on, n, via := a.egress.state()
			st["proxy"] = map[string]any{"on": on, "conns": n, "via": via, "socks": socksPort, "http": httpPort,
				"chrome": chromePath() != "", "claude": claudeAvailable()}
		}
		reply(w, st)
	})
	api("/api/import", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var p *Profile
		var err error
		if strings.Contains(string(raw), `"most-invite"`) { // приглашение: зарегистрировать это устройство на сервере
			p, err = enrollByInvite(raw)
		} else {
			p, err = importProfile(raw)
		}
		if err != nil {
			fail(w, err)
			return
		}
		log.Printf("профиль «%s» импортирован", p.Name)
		a.mu.Lock()
		old := a.fwd
		a.mu.Unlock()
		if old != nil { // новый профиль — старые каналы выключить полностью
			a.egress.Stop()
			old.stop()
			a.mu.Lock()
			a.fwd = nil
			a.mu.Unlock()
		}
		a.use(p)
		reply(w, map[string]string{"ok": "1"})
	})
	// страница программы на GitHub — в браузере (в своём окне WebView2 ссылка увела бы окно программы)
	api("/api/open-repo", func(w http.ResponseWriter, r *http.Request) {
		openLink(repoURL)
		reply(w, map[string]any{"ok": "1"})
	})
	// пароль пользователя рабочего стола — по кнопке «Показать»/«Скопировать» на главной (в /api/state его нет)
	api("/api/rdp-password", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		p := a.profile
		a.mu.Unlock()
		if p == nil || p.RDPUser == "" {
			fail(w, errNoProfile)
			return
		}
		reply(w, map[string]any{"user": p.RDPUser, "password": p.RDPPassword})
	})
	api("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		p := a.profile
		a.mu.Unlock()
		if p == nil {
			fail(w, errNoProfile)
			return
		}
		if p.RDPUser == "" {
			fail(w, strErr("на этом сервере не ставился рабочий стол — доступен только «Через наш канал»"))
			return
		}
		a.userDisc.Store(false)
		if err := a.openRDP(p); err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]any{"ok": "1", "rdp": rdpHint(p)})
	})
	api("/api/disconnect", func(w http.ResponseWriter, r *http.Request) {
		a.userDisc.Store(true) // окно закрывает человек — автооткрытие не нужно
		a.mu.Lock()
		cmd, f := a.mstsc, a.fwd
		a.mu.Unlock()
		if cmd != nil && a.rdpOpen.Load() {
			_ = cmd.Kill()
		}
		if f != nil {
			f.disconnect()
		}
		log.Printf("отключено кнопкой (рабочий стол на сервере остаётся)")
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		f := a.fwd
		a.mu.Unlock()
		if f == nil {
			reply(w, statsView{})
			return
		}
		reply(w, f.stats.view())
	})
	api("/api/proxy", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ On bool }
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.mu.Lock()
		f := a.fwd
		a.mu.Unlock()
		if f == nil {
			fail(w, errNoProfile)
			return
		}
		if req.On {
			if err := a.egress.Start(f); err != nil {
				fail(w, err)
				return
			}
		} else {
			a.egress.Stop()
		}
		saveSettings(Settings{Proxy: req.On})
		reply(w, map[string]bool{"on": req.On})
	})
	api("/api/launch", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ What string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if on, _, _ := a.egress.state(); !on {
			fail(w, strErr("сначала включите «Через наш канал»"))
			return
		}
		var err error
		switch req.What {
		case "chrome":
			err = launchChrome()
		case "claude":
			err = launchClaude()
		default:
			err = strErr("неизвестная программа")
		}
		if err != nil {
			fail(w, err)
			return
		}
		log.Printf("запущен %s через канал", req.What)
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/exitip", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		f := a.fwd
		a.mu.Unlock()
		if f == nil {
			fail(w, errNoProfile)
			return
		}
		if on, _, _ := a.egress.state(); !on {
			fail(w, strErr("«Через наш канал» выключен"))
			return
		}
		ip, via, err := a.egress.exitIP()
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"ip": ip, "via": via})
	})
	adminReq := func(w http.ResponseWriter) (*Profile, []Channel, bool) {
		a.mu.Lock()
		p, f := a.profile, a.fwd
		a.mu.Unlock()
		if p == nil || f == nil {
			fail(w, errNoProfile)
			return nil, nil, false
		}
		if p.AdminToken == "" {
			fail(w, strErr("управление устройствами доступно только на устройствах владельца"))
			return nil, nil, false
		}
		return p, f.usable(), true
	}
	api("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		p, chs, ok := adminReq(w)
		if !ok {
			return
		}
		var list []map[string]any
		if err := adminCall(chs, "GET", "/api/devices", p.AdminToken, nil, &list); err != nil {
			fail(w, err)
			return
		}
		var inv []map[string]any // ожидающие приглашения (старый сервер списка не отдаёт — тогда пусто)
		_ = adminCall(chs, "GET", "/api/invites", p.AdminToken, nil, &inv)
		reply(w, map[string]any{"devices": list, "invites": inv, "now": time.Now().Unix()})
	})
	api("/api/devices/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Pub string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		p, chs, ok := adminReq(w)
		if !ok {
			return
		}
		var out map[string]any
		if err := adminCall(chs, "DELETE", "/api/devices?pub="+url.QueryEscape(req.Pub), p.AdminToken, nil, &out); err != nil {
			fail(w, err)
			return
		}
		log.Printf("устройство удалено")
		reply(w, out)
	})
	api("/api/invites/cancel", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		p, chs, ok := adminReq(w)
		if !ok {
			return
		}
		var out map[string]any
		if err := adminCall(chs, "DELETE", "/api/invites?id="+url.QueryEscape(req.ID), p.AdminToken, nil, &out); err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/invite", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		p, chs, ok := adminReq(w)
		if !ok {
			return
		}
		var inv map[string]any
		if err := adminCall(chs, "POST", "/api/invites", p.AdminToken, map[string]string{"name": req.Name}, &inv); err != nil {
			fail(w, err)
			return
		}
		log.Printf("создано приглашение для устройства «%s»", req.Name)
		reply(w, inv)
	})
	// ----- обновления -----
	api("/api/update", func(w http.ResponseWriter, r *http.Request) { reply(w, a.upd.state()) })
	api("/api/update/check", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		f := a.fwd
		a.mu.Unlock()
		if f == nil {
			fail(w, errNoProfile)
			return
		}
		a.upd.Check(f.usable())
		reply(w, a.upd.state())
	})
	api("/api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		if err := a.upd.Apply(); err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"ok": "1"})
	})
	// ----- несколько серверов -----
	api("/api/profiles", func(w http.ResponseWriter, r *http.Request) { reply(w, listProfiles()) })
	api("/api/profiles/use", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		p, err := profileByID(req.ID)
		if err != nil {
			fail(w, err)
			return
		}
		if err := setActive(req.ID); err != nil {
			fail(w, err)
			return
		}
		a.switchTo(p)
		log.Printf("переключились на сервер «%s»", p.Name)
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/profiles/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := deleteProfile(req.ID); err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"ok": "1"})
	})
	// ----- мастер подготовки сервера -----
	api("/api/setup/inspect", func(w http.ResponseWriter, r *http.Request) {
		var req SetupReq
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
		if req.Host == "" || req.User == "" {
			fail(w, strErr("укажите адрес сервера и пользователя"))
			return
		}
		info, err := inspect(req)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, info)
	})
	api("/api/setup/start", func(w http.ResponseWriter, r *http.Request) {
		var req SetupReq
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
		if req.HostKey == "" {
			fail(w, strErr("сначала проверьте сервер и подтвердите его ключ"))
			return
		}
		if err := a.setup.Start(req); err != nil {
			fail(w, err)
			return
		}
		log.Printf("мастер: подготовка сервера %s запущена", req.Host)
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/setup/state", func(w http.ResponseWriter, r *http.Request) { reply(w, a.setup.state()) })
	api("/api/setup/reset", func(w http.ResponseWriter, r *http.Request) { a.setup.Reset(); reply(w, map[string]string{"ok": "1"}) })
	api("/api/setup/reattach", func(w http.ResponseWriter, r *http.Request) {
		if err := a.setup.Reattach(); err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/setup/profile", func(w http.ResponseWriter, r *http.Request) {
		b, err := a.setup.profileJSON()
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	api("/api/setup/use", func(w http.ResponseWriter, r *http.Request) {
		b, err := a.setup.profileJSON()
		if err != nil {
			fail(w, err)
			return
		}
		p, err := importProfile(b)
		if err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		old := a.fwd
		a.fwd = nil
		a.mu.Unlock()
		if old != nil {
			a.egress.Stop()
			old.stop()
		}
		a.use(p)
		a.setup.Reset() // профиль сохранён в программе — ход установки, ключи и пароль из памяти мастера убрать
		log.Printf("мастер: переключились на сервер «%s»", p.Name)
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/autostart", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ On bool }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := setAutostart(req.On); err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]bool{"on": autostartEnabled()})
	})
	api("/api/mode", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Mode string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.mu.Lock()
		if a.fwd != nil {
			a.fwd.mu.Lock()
			a.fwd.mode = req.Mode
			a.fwd.mu.Unlock()
		}
		a.mu.Unlock()
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/probe", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		f := a.fwd
		a.mu.Unlock()
		if f != nil {
			f.probeAll()
		}
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/exchange", func(w http.ResponseWriter, r *http.Request) {
		openFolder(exchangeDir())
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/log", func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile(filepath.Join(appDir(), "most.log"))
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 60 {
			lines = lines[len(lines)-60:]
		}
		reply(w, map[string]any{"lines": lines})
	})
	// показать окно (повторный запуск exe просит об этом работающий «Мост» — окна открывает один процесс по очереди)
	api("/api/show", func(w http.ResponseWriter, r *http.Request) {
		if showHook != nil {
			go showHook()
		}
		reply(w, map[string]string{"ok": "1"})
	})
	api("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]string{"ok": "1"})
		log.Printf("выход по кнопке")
		go func() { time.Sleep(300 * time.Millisecond); platformQuit(); time.Sleep(200 * time.Millisecond); os.Exit(0) }() // убрать значок из трея
	})
}

// selfProbe проверяет каждый канал профиля (без сохранения профиля и без окна) и печатает итог в stdout.
func selfProbe(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		os.Stdout.WriteString("нет файла профиля: " + err.Error() + "\n")
		return
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		os.Stdout.WriteString("профиль не читается: " + err.Error() + "\n")
		return
	}
	if p.AWG != nil && p.AWG.Host == "" {
		p.AWG.Host = "10.77.0.1"
	}
	for _, c := range buildChannels(&p) {
		for i := 1; i <= 3; i++ {
			d, err := probe(c)
			if err != nil {
				os.Stdout.WriteString(c.ID() + " #" + itoa(i) + ": ОШИБКА " + err.Error() + "\n")
				c.Reset()
			} else {
				os.Stdout.WriteString(c.ID() + " #" + itoa(i) + ": OK " + itoa(int(d.Milliseconds())) + " мс\n")
			}
		}
	}
}


func selfEgress(path, id string) {
	raw, _ := os.ReadFile(path)
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		os.Stdout.WriteString("профиль не читается\n")
		return
	}
	if p.AWG != nil && p.AWG.Host == "" {
		p.AWG.Host = "10.77.0.1"
	}
	for _, c := range buildChannels(&p) {
		if c.ID() != id {
			continue
		}
		eg, ok := c.(Egresser)
		if !ok {
			os.Stdout.WriteString("канал без выхода в интернет\n")
			return
		}
		if os.Getenv("MOST_PROBE_FIRST") != "" { // как в окне программы: сначала проверка канала RDP
			d, perr := probe(c)
			os.Stdout.WriteString("проверка RDP: " + d.String() + " " + fmt.Sprint(perr) + "\n")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		conn, err := eg.DialOut(ctx, "api.ipify.org", 80)
		if err != nil {
			os.Stdout.WriteString("DialOut: " + err.Error() + "\n")
			return
		}
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		conn.Write([]byte("GET / HTTP/1.1\r\nHost: api.ipify.org\r\nConnection: close\r\n\r\n"))
		b, err := io.ReadAll(conn)
		os.Stdout.WriteString("ответ: " + string(b) + "\nошибка: " + fmt.Sprint(err) + "\n")
	}
}

type strErr string

func (e strErr) Error() string { return string(e) }

// showHook — как показать окно на этой платформе (Windows: значок у часов; на остальных — оболочка сама)
var showHook func()

const errNoProfile = strErr("сначала загрузите файл профиля")

