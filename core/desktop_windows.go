//go:build windows

package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RunDesktop — запуск программы на Windows: самопроверки по ключам командной строки, одна копия,
// окно Edge, значок у часов, автообновление.
func RunDesktop() {
	logf, _ := os.OpenFile(filepath.Join(appDir(), "most.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if logf != nil {
		log.SetOutput(logf)
	}
	if len(os.Args) == 4 && os.Args[1] == "-appx" { // проверка запуска пакетного приложения: most.exe -appx <AUMID> <аргументы>
		if err := activateAppx(os.Args[2], os.Args[3]); err != nil {
			os.Stdout.WriteString(err.Error() + "\n")
		} else {
			os.Stdout.WriteString("запущено\n")
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "-enroll" { // самопроверка приглашения: most.exe -enroll приглашение.json профиль-выход.json
		raw, _ := os.ReadFile(os.Args[2])
		p, err := enrollRaw(raw)
		if err != nil {
			os.Stdout.WriteString("ошибка: " + err.Error() + "\n")
			return
		}
		b, _ := json.MarshalIndent(p, "", " ")
		_ = os.WriteFile(os.Args[3], b, 0o600)
		os.Stdout.WriteString("профиль получен: " + p.Name + "\n")
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "-egress" { // самопроверка выхода: most.exe -egress profile.json cdn|awg|xray
		selfEgress(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "-probe" { // самопроверка: most.exe -probe profile.json > итог.txt
		selfProbe(os.Args[2])
		return
	}
	tokenPath := filepath.Join(appDir(), "ui.token")
	ln, err := net.Listen("tcp", uiAddr)
	if err != nil { // программа уже запущена
		t, e := os.ReadFile(tokenPath)
		if e != nil {
			return
		}
		tok := strings.TrimSpace(string(t))
		if replaceOld(tok) { // запущена другая версия — она выключена, занимаем её место
			for i := 0; i < 30 && err != nil; i++ {
				time.Sleep(200 * time.Millisecond)
				ln, err = net.Listen("tcp", uiAddr)
			}
		}
		if err != nil { // та же версия — попросить её показать окно (она открывает окна по очереди), иначе открыть самим
			req, _ := http.NewRequest("POST", "http://"+uiAddr+"/api/show", strings.NewReader("{}"))
			req.Header.Set("X-Token", tok)
			req.Header.Set("Content-Type", "application/json")
			if r, e := (&http.Client{Timeout: 3 * time.Second}).Do(req); e == nil && r.StatusCode == 200 {
				r.Body.Close()
				return
			}
			openWindow("http://" + uiAddr + "/?t=" + tok)
			return
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	app := &App{token: hex.EncodeToString(b), started: time.Now()}
	_ = os.WriteFile(tokenPath, []byte(app.token), 0o600)
	log.Printf("Мост %s запущен", version)

	if p, err := loadProfile(); err == nil {
		app.use(p)
	}
	mux := http.NewServeMux()
	app.routes(mux)
	go func() { _ = http.Serve(ln, mux) }()
	if !(len(os.Args) == 2 && os.Args[1] == "-hidden") { // -hidden — автозапуск с Windows, без окна
		launchWindow("http://" + uiAddr + "/?t=" + app.token)
	}
	if autostartEnabled() { // после обновления exe лежит в другом месте — автозапуск указывает на текущий
		_ = setAutostart(true)
	}
	showHook = app.openUI
	go app.upd.loop(app)
	go app.paramsLoop()
	app.runTray() // значок у часов; работает, пока программу не выключат
}

// replaceOld — если уже работает другая версия «Моста» (например, старая), выключить её. true — выключена.
func replaceOld(tok string) bool {
	c := http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", "http://"+uiAddr+"/api/state", nil)
	req.Header.Set("X-Token", tok)
	r, err := c.Do(req)
	if err != nil {
		return false
	}
	var st struct {
		Version string
		Pid     int
	}
	_ = json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if st.Version == version {
		return false
	}
	q, _ := http.NewRequest("POST", "http://"+uiAddr+"/api/quit", strings.NewReader("{}"))
	q.Header.Set("X-Token", tok)
	if r, err := c.Do(q); err == nil {
		r.Body.Close()
	}
	// подстраховка: если прежняя копия не завершилась сама за 4 с — завершить её принудительно
	if st.Pid > 0 && st.Pid != os.Getpid() {
		if p, err := os.FindProcess(st.Pid); err == nil {
			done := make(chan struct{})
			go func() { _, _ = p.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				_ = p.Kill()
				log.Printf("прежняя версия %s не завершилась сама — завершена принудительно", st.Version)
			}
		}
	}
	log.Printf("выключена прежняя версия %s, запускается %s", st.Version, version)
	return true
}

// openWindow — окно программы: Edge в режиме приложения (без адресной строки), иначе браузер по умолчанию.
// openWindow — показать окно: уже открытое вывести вперёд, иначе открыть (значок у часов, повторный запуск).
func openWindow(url string) {
	if showNative(url) { // встроенное окно (WebView2) — одно на всё время работы
		return
	}
	// запасной путь — окно Edge (если WebView2 в системе нет)
	winMu.Lock() // несколько щелчков подряд — по очереди, а не наперегонки
	defer winMu.Unlock()
	if focusWindow() { // окно уже открыто — показать его, а не плодить копии
		return
	}
	// окно только что запущено, но Edge ещё не успел его показать (около секунды) — дождаться и показать его,
	// а не запускать второе (несколько быстрых щелчков по значку открывали столько же окон)
	if time.Since(lastLaunch) < 8*time.Second {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
			if focusWindow() {
				return
			}
		}
		return
	}
	launchWindow(url)
}

var (
	winMu      sync.Mutex
	lastLaunch time.Time
)

// launchWindow — открыть новое окно. При запуске программы — всегда новое: окно прежней версии (после
// обновления) ещё висит пару секунд и закрывается само, выводить его вперёд нельзя.
func launchWindow(url string) {
	if showNative(url) {
		return
	}
	lastLaunch = time.Now()
	// размер окна — как у телефона в ландшафте; на маленьком экране Edge ужмёт сам
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
		edge := filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`)
		if _, err := os.Stat(edge); err == nil {
			if exec.Command(edge, "--app="+url, "--window-size=960,680").Start() == nil {
				return
			}
		}
	}
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
