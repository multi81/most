package core

// Автообновление: сначала GitHub Releases (github.com/multi81/most, последний выпуск: latest.json и most.exe),
// если GitHub недоступен — сервер (most-admin /api/update) через свои каналы. Смена сервера обновлений не касается.
// В обоих случаях программа проверяет размер, SHA-256 и подпись Ed25519 (открытый ключ зашит ниже) и только потом предлагает обновиться.
// Новая версия кладётся в %LOCALAPPDATA%\Most\bin\most-<версия>.exe и при запуске сама сменяет работающую.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const updatePubKey = "qiQs3NzBzkiH5ll1n2M+559KzD4qMFXwLlwqZNrsamk=" // открытая часть ключа подписи сборок

type UpdateInfo struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Sig     string `json:"sig"` // Ed25519 над строкой "most|<версия>|<sha256>"
	Notes   string `json:"notes"`
}

type Updater struct {
	mu      sync.Mutex
	avail   *UpdateInfo
	file    string // скачанный и проверенный exe
	checked time.Time
	err     string
	busy    bool
	source  string // откуда пришёл ответ последней проверки: «GitHub» или «сервер»
}

// newer: "0.6.1" > "0.6.0"
func newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}


func verifySig(u *UpdateInfo) bool {
	pub, err := base64.StdEncoding.DecodeString(updatePubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(u.Sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, []byte("most|"+u.Version+"|"+strings.ToLower(u.SHA256)), sig)
}

// fetchVia — GET к most-admin через первый живой канал, ответ целиком (до limit байт).
// fetchVia — скачать файл с most-admin через каналы. Каналы — по надёжности (AWG, CDN, Reality: у Reality
// подключение «проходит» сразу, даже когда дорога режется); оборвалось — докачать с того же места (Range)
// через следующий канал, до трёх кругов. Раньше брался первый канал по списку без повтора — 37 МБ
// обрывались посередине и сборка считалась повреждённой.
func fetchVia(chs []Channel, path string, limit int64) ([]byte, error) {
	rank := map[string]int{"awg": 0, "cdn": 1, "xray": 2}
	var list []PortDialer
	for _, c := range chs {
		if pd, ok := c.(PortDialer); ok {
			list = append(list, pd)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return rank[list[i].ID()] < rank[list[j].ID()] })
	var buf []byte
	var lastErr error = errors.New("нет живого канала до сервера")
	for round := 0; round < 3; round++ {
		for _, pd := range list {
			pd := pd
			tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return pd.DialPort(ctx, adminPort) }}
			cl := http.Client{Transport: tr, Timeout: 10 * time.Minute}
			req, _ := http.NewRequest("GET", "http://most-admin"+path, nil)
			if len(buf) > 0 {
				req.Header.Set("Range", "bytes="+strconv.Itoa(len(buf))+"-")
			}
			r, err := cl.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			switch r.StatusCode {
			case 404:
				r.Body.Close()
				return nil, nil
			case 200:
				buf = buf[:0] // сервер отдал файл целиком (без докачки)
			case 206:
			default:
				r.Body.Close()
				lastErr = errors.New(r.Status)
				continue
			}
			b, err := io.ReadAll(io.LimitReader(r.Body, limit-int64(len(buf))))
			r.Body.Close()
			buf = append(buf, b...)
			if err == nil {
				return buf, nil
			}
			lastErr = err
			log.Printf("загрузка %s через %s оборвалась на %d байтах: %v — докачиваю", path, pd.ID(), len(buf), err)
		}
	}
	return buf, lastErr
}

// Check — узнать о новой версии и сразу скачать и проверить её.
func (u *Updater) Check(chs []Channel) {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return
	}
	u.busy = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.checked = time.Now(); u.mu.Unlock() }()
	setErr := func(e string) { u.mu.Lock(); u.err = e; u.mu.Unlock() }
	var info UpdateInfo
	fromGH := ghInfo(&info) == nil
	src := "GitHub"
	if !fromGH {
		src = "сервер"
	}
	if !fromGH { // GitHub недоступен — спросить сервер через каналы
		info = UpdateInfo{}
		if err := adminCall(chs, "GET", "/api/update", "", nil, &info); err != nil {
			if strings.Contains(err.Error(), "обновлений нет") {
				setErr("")
				u.mu.Lock()
				u.source = src
				u.mu.Unlock()
				return
			}
			setErr("проверка не удалась: " + err.Error())
			return
		}
	}
	u.mu.Lock()
	u.source = src
	u.mu.Unlock()
	if !newer(info.Version, version) {
		setErr("")
		return
	}
	if !verifySig(&info) {
		setErr("сборка " + info.Version + " с неверной подписью — не устанавливаю")
		log.Printf("обновление %s: подпись не сошлась", info.Version)
		return
	}
	dst := filepath.Join(binDir(), "most-"+info.Version+".exe")
	if b, err := os.ReadFile(dst); err == nil && sumHex(b) == strings.ToLower(info.SHA256) {
		u.mu.Lock()
		u.avail, u.file, u.err = &info, dst, ""
		u.mu.Unlock()
		return
	}
	var b []byte
	var err error
	if fromGH {
		b, err = ghFetch(ghBase+"most.exe", info.Size+1)
	}
	if !fromGH || err != nil || int64(len(b)) != info.Size || sumHex(b) != strings.ToLower(info.SHA256) {
		b, err = fetchVia(chs, "/api/update/file", info.Size+1)
	}
	if err != nil || int64(len(b)) != info.Size || sumHex(b) != strings.ToLower(info.SHA256) {
		setErr("скачанная сборка " + info.Version + " повреждена — попробую позже")
		return
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		setErr("не удалось сохранить обновление: " + err.Error())
		return
	}
	log.Printf("обновление %s скачано и проверено (подпись, sha256)", info.Version)
	u.mu.Lock()
	u.avail, u.file, u.err = &info, dst, ""
	u.mu.Unlock()
}

// repoURL — страница программы: исходный код и выпуски (показывается в окне программы).
const repoURL = "https://github.com/multi81/most"

// ghBase — файлы последнего выпуска на GitHub (переадресация на хранилище GitHub).
const ghBase = "https://github.com/multi81/most/releases/latest/download/"

var ghClient = &http.Client{Timeout: 10 * time.Minute}

// ghInfo — latest.json последнего выпуска на GitHub.
func ghInfo(info *UpdateInfo) error {
	b, err := ghFetch(ghBase+"latest.json", 64<<10)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, info); err != nil || info.Version == "" {
		return errors.New("latest.json на GitHub не читается")
	}
	return nil
}

// ghFetch — скачать файл напрямую, с докачкой (Range) при обрыве, до трёх попыток.
func ghFetch(url string, limit int64) ([]byte, error) {
	var buf []byte
	var lastErr error
	for try := 0; try < 3; try++ {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "most/"+version)
		if len(buf) > 0 {
			req.Header.Set("Range", "bytes="+strconv.Itoa(len(buf))+"-")
		}
		r, err := ghClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		switch r.StatusCode {
		case 200:
			buf = buf[:0]
		case 206:
		default:
			r.Body.Close()
			return nil, errors.New("GitHub: " + r.Status)
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, limit-int64(len(buf))))
		r.Body.Close()
		buf = append(buf, b...)
		if err == nil {
			return buf, nil
		}
		lastErr = err
	}
	return buf, lastErr
}

func sumHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (u *Updater) state() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := map[string]any{"current": version, "busy": u.busy, "error": u.err, "source": u.source, "repo": repoURL}
	if !u.checked.IsZero() {
		st["checked"] = u.checked.Format("15:04")
	}
	if u.avail != nil {
		st["available"] = u.avail.Version
		st["notes"] = u.avail.Notes
	}
	return st
}

// Apply — запустить новую версию: она сама выключит эту и займёт её место (см. replaceOld).
func (u *Updater) Apply() error {
	u.mu.Lock()
	f := u.file
	u.mu.Unlock()
	if f == "" {
		return errors.New("обновление ещё не скачано")
	}
	b, err := os.ReadFile(f) // перепроверить перед запуском: файл мог подменить кто-то другой
	if err != nil || sumHex(b) != strings.ToLower(u.avail.SHA256) {
		return errors.New("файл обновления изменился — скачаю заново")
	}
	log.Printf("обновляюсь до %s", u.avail.Version)
	return startExe(f)
}

func (u *Updater) loop(a *App) {
	if !updatesSupported() { // на Android обновления — через установку APK
		return
	}
	time.Sleep(45 * time.Second) // дать каналам подняться
	for {
		a.mu.Lock()
		f := a.fwd
		a.mu.Unlock()
		if f != nil {
			u.Check(f.usable())
		}
		time.Sleep(6 * time.Hour)
	}
}
