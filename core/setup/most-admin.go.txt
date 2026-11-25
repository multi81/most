// most-admin — служба управления устройствами «Моста» на сервере (2026-11-15, Вера).
// Слушает только 127.0.0.1:8787 и 10.77.0.1:8787 (снаружи недоступна; изнутри — через каналы «Моста»).
//   администратор (токен из профиля владельца, заголовок X-Admin):
//     GET    /api/devices            — устройства (пиры AWG): имя, адрес, последнее рукопожатие, трафик
//     POST   /api/invites {name}     — приглашение: одноразовый код на 24 ч + данные каналов без ключа AWG
//     DELETE /api/devices?pub=…      — удалить устройство (из конфига и из работающего awg0)
//   новое устройство (код приглашения):
//     POST   /api/enroll {code,pub}  — регистрирует открытый ключ AWG устройства, выдаёт полный профиль
// Закрытый ключ устройства сюда не приходит никогда. Конфиг /etc/most-admin/config.json (600, секреты),
// приглашения /var/lib/most-admin/invites.json.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const awgConf = "/etc/amnezia/amneziawg/awg0.conf"
const invitesPath = "/var/lib/most-admin/invites.json"

type Config struct {
	Token       string          `json:"token"`
	Name        string          `json:"name"`
	Server      string          `json:"server"`
	RDPUser     string          `json:"rdp_user"`
	RDPPassword string          `json:"rdp_password"`
	AWGHost     string          `json:"awg_host"`
	Endpoint    string          `json:"endpoint"` // адрес:порт AWG снаружи
	SSHPort     int             `json:"ssh_port,omitempty"` // порт SSH для канала SSH устройств (по умолчанию 22)
	Xray        json.RawMessage `json:"xray"`
}

type Invite struct {
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
	Used    bool      `json:"used"`
}

var (
	cfg Config
	mu  sync.Mutex
)

func main() {
	b, err := os.ReadFile("/etc/most-admin/config.json")
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil || len(cfg.Token) < 32 {
		log.Fatal("конфиг повреждён или токен короткий")
	}
	_ = os.MkdirAll("/var/lib/most-admin", 0o700)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/devices", admin(devices))
	mux.HandleFunc("/api/invites", admin(invites))
	mux.HandleFunc("/api/enroll", enroll)
	// текущие параметры маскировки Xray (имя сайта Reality, ключ, CDN) — «Мост» сам обновляет по ним профиль;
	// без идентификаторов пользователей; служба слушает только изнутри канала
	mux.HandleFunc("/api/params", func(w http.ResponseWriter, r *http.Request) {
		var x map[string]any
		if err := json.Unmarshal(cfg.Xray, &x); err != nil {
			fail(w, 500, "нет параметров")
			return
		}
		delete(x, "uuid")
		delete(x, "proxy_uuid")
		reply(w, 200, x)
	})
	// обновления «Моста»: подписанные сборки кладёт владелец в /var/lib/most-admin/update/ (latest.json + most.exe);
	// подпись проверяет сама программа (открытый ключ зашит в неё), здесь — только раздача
	mux.HandleFunc("/api/update", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile("/var/lib/most-admin/update/latest.json")
		if err != nil {
			fail(w, 404, "обновлений нет")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/api/update/file", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "/var/lib/most-admin/update/most.exe")
	})
	srv := func(addr string) {
		for {
			ln, err := net.Listen("tcp", addr)
			if err != nil { // 10.77.0.1 появляется вместе с awg0 — ждём
				time.Sleep(5 * time.Second)
				continue
			}
			log.Printf("слушаю %s", addr)
			_ = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
		}
	}
	go srv("10.77.0.1:8787")
	srv("127.0.0.1:8787")
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { reply(w, code, map[string]string{"error": msg}) }

func admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin")), []byte(cfg.Token)) != 1 {
			time.Sleep(500 * time.Millisecond)
			fail(w, 403, "нет доступа")
			return
		}
		h(w, r)
	}
}

// ---------- устройства ----------

type Device struct {
	Name      string `json:"name"`
	Pub       string `json:"pub"`
	IP        string `json:"ip"`
	Endpoint  string `json:"endpoint"`
	Handshake int64  `json:"handshake"` // unix, 0 — не было
	Rx        int64  `json:"rx"`
	Tx        int64  `json:"tx"`
	Fixed     bool   `json:"fixed"` // служебные (шлюз офиса) — не удалять из программы
}

type peerConf struct{ name, pub, allowed string }

// readPeers — пиры из конфига с именами из комментария «# имя» перед [Peer].
func readPeers() ([]peerConf, error) {
	f, err := os.Open(awgConf)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []peerConf
	var cur *peerConf
	last := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "#"):
			last = strings.TrimSpace(strings.TrimPrefix(l, "#"))
		case l == "[Peer]":
			out = append(out, peerConf{name: last})
			cur = &out[len(out)-1]
			last = ""
		case cur != nil && strings.HasPrefix(l, "PublicKey"):
			cur.pub = strings.TrimSpace(strings.SplitN(l, "=", 2)[1])
		case cur != nil && strings.HasPrefix(l, "AllowedIPs"):
			cur.allowed = strings.TrimSpace(strings.SplitN(l, "=", 2)[1])
		}
	}
	return out, sc.Err()
}

func devices(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		peers, err := readPeers()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		live := map[string][]string{}
		out, _ := exec.Command("awg", "show", "awg0", "dump").Output()
		for i, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if i == 0 {
				continue
			}
			f := strings.Split(l, "\t")
			if len(f) >= 7 {
				live[f[0]] = f
			}
		}
		var list []Device
		for _, p := range peers {
			d := Device{Name: p.name, Pub: p.pub, IP: strings.Split(p.allowed, ",")[0], Fixed: strings.Contains(p.allowed, ",")}
			if f, ok := live[p.pub]; ok {
				if f[2] != "(none)" {
					d.Endpoint = f[2]
				}
				d.Handshake, _ = strconv.ParseInt(f[4], 10, 64)
				d.Rx, _ = strconv.ParseInt(f[5], 10, 64)
				d.Tx, _ = strconv.ParseInt(f[6], 10, 64)
			}
			list = append(list, d)
		}
		reply(w, 200, list)
	case http.MethodDelete:
		pub := r.URL.Query().Get("pub")
		peers, _ := readPeers()
		var found *peerConf
		for i := range peers {
			if peers[i].pub == pub {
				found = &peers[i]
			}
		}
		if found == nil {
			fail(w, 404, "нет такого устройства")
			return
		}
		if strings.Contains(found.allowed, ",") {
			fail(w, 400, "служебное устройство (шлюз офиса) из программы не удаляется")
			return
		}
		if err := removePeer(pub); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if cfg.RDPUser != "" {
			if err := removeSSHKey(cfg.RDPUser, pub); err != nil {
				log.Printf("ключ SSH устройства %q не удалён: %v", found.name, err)
			}
		}
		log.Printf("удалено устройство %q %s", found.name, found.allowed)
		reply(w, 200, map[string]string{"ok": "1"})
	default:
		fail(w, 405, "метод")
	}
}

// removePeer — убрать блок пира из конфига (вместе с комментарием-именем) и из работающего интерфейса.
func removePeer(pub string) error {
	b, err := os.ReadFile(awgConf)
	if err != nil {
		return err
	}
	_ = os.WriteFile("/root/claude-changes/awg0.conf.before-delete-"+time.Now().Format("0102-150405"), b, 0o600)
	blocks := regexp.MustCompile(`\n(?:#[^\n]*\n)?\[Peer\]\n`).Split("\n"+string(b), -1)
	heads := regexp.MustCompile(`\n(?:#[^\n]*\n)?\[Peer\]\n`).FindAllString("\n"+string(b), -1)
	var sb strings.Builder
	sb.WriteString(strings.TrimPrefix(blocks[0], "\n"))
	for i, blk := range blocks[1:] {
		if strings.Contains(blk, "PublicKey = "+pub) {
			continue
		}
		sb.WriteString(heads[i])
		sb.WriteString(blk)
	}
	if err := os.WriteFile(awgConf, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	return exec.Command("awg", "set", "awg0", "peer", pub, "remove").Run()
}

// ---------- приглашения ----------

func loadInvites() map[string]*Invite {
	m := map[string]*Invite{}
	if b, err := os.ReadFile(invitesPath); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	for k, v := range m { // просроченные и использованные старше суток — убрать
		if time.Since(v.Expires) > 24*time.Hour {
			delete(m, k)
		}
	}
	return m
}

func saveInvites(m map[string]*Invite) {
	b, _ := json.MarshalIndent(m, "", " ")
	_ = os.WriteFile(invitesPath, b, 0o600)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var nameRe = regexp.MustCompile(`^[\p{L}\p{N} ._-]{1,40}$`)

func invites(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet: // ожидающие приглашения (ещё не использованы и не просрочены) — без кодов
		mu.Lock()
		m := loadInvites()
		mu.Unlock()
		type pend struct {
			ID      string    `json:"id"`
			Name    string    `json:"name"`
			Expires time.Time `json:"expires"`
		}
		list := []pend{}
		for code, inv := range m {
			if !inv.Used && time.Now().Before(inv.Expires) {
				list = append(list, pend{inviteID(code), inv.Name, inv.Expires})
			}
		}
		reply(w, 200, list)
		return
	case http.MethodDelete: // отменить приглашение
		id := r.URL.Query().Get("id")
		mu.Lock()
		m := loadInvites()
		found := false
		for code, inv := range m {
			if inviteID(code) == id && !inv.Used {
				delete(m, code)
				found = true
				log.Printf("приглашение для %q отменено", inv.Name)
			}
		}
		saveInvites(m)
		mu.Unlock()
		if !found {
			fail(w, 404, "нет такого приглашения")
			return
		}
		reply(w, 200, map[string]string{"ok": "1"})
		return
	case http.MethodPost:
	default:
		fail(w, 405, "метод")
		return
	}
	var req struct{ Name string }
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	req.Name = strings.TrimSpace(req.Name)
	if !nameRe.MatchString(req.Name) {
		fail(w, 400, "имя устройства: буквы, цифры, пробел, до 40 знаков")
		return
	}
	mu.Lock()
	m := loadInvites()
	code := randHex(16)
	m[code] = &Invite{Name: req.Name, Expires: time.Now().Add(24 * time.Hour)}
	saveInvites(m)
	mu.Unlock()
	log.Printf("приглашение для %q", req.Name)
	// файл приглашения: только каналы без AWG (xray/CDN) и код — этого хватает, чтобы новое устройство дошло сюда
	reply(w, 200, map[string]any{"kind": "most-invite", "name": cfg.Name, "device": req.Name, "code": code,
		"server": cfg.Server, "xray": cfg.Xray, "expires": m[code].Expires})
}

// inviteID — короткий несекретный идентификатор приглашения (код приглашения — секрет, в список не отдаётся)
func inviteID(code string) string { h := sha256.Sum256([]byte(code)); return hex.EncodeToString(h[:6]) }

// ---------- регистрация нового устройства ----------

func validPub(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

func nextIP(peers []peerConf) (string, error) {
	used := map[string]bool{}
	for _, p := range peers {
		for _, a := range strings.Split(p.allowed, ",") {
			used[strings.TrimSpace(strings.Split(a, "/")[0])] = true
		}
	}
	for i := 20; i < 250; i++ {
		ip := "10.77.0." + strconv.Itoa(i)
		if !used[ip] {
			return ip, nil
		}
	}
	return "", errors.New("свободных адресов нет")
}

func obfLines() string {
	b, _ := os.ReadFile(awgConf)
	var sb strings.Builder
	inIface := false
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			inIface = t == "[Interface]"
			continue
		}
		k := strings.TrimSpace(strings.SplitN(t, "=", 2)[0])
		if inIface && regexp.MustCompile(`^(Jc|Jmin|Jmax|S[1-4]|H[1-4]|I[1-5])$`).MatchString(k) {
			sb.WriteString(t + "\n")
		}
	}
	return sb.String()
}

func enroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "метод")
		return
	}
	var req struct{ Code, Pub, SSH string }
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	mu.Lock()
	defer mu.Unlock()
	m := loadInvites()
	inv := m[req.Code]
	if inv == nil || inv.Used || time.Now().After(inv.Expires) {
		time.Sleep(time.Second)
		fail(w, 403, "приглашение недействительно или уже использовано")
		return
	}
	if !validPub(req.Pub) {
		fail(w, 400, "неверный открытый ключ")
		return
	}
	peers, err := readPeers()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for _, p := range peers {
		if p.pub == req.Pub {
			fail(w, 409, "этот ключ уже зарегистрирован")
			return
		}
	}
	ip, err := nextIP(peers)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	pskOut, err := exec.Command("awg", "genpsk").Output()
	if err != nil {
		fail(w, 500, "genpsk: "+err.Error())
		return
	}
	psk := strings.TrimSpace(string(pskOut))
	srvPubOut, _ := exec.Command("awg", "show", "awg0", "public-key").Output()
	srvPub := strings.TrimSpace(string(srvPubOut))
	b, _ := os.ReadFile(awgConf)
	_ = os.WriteFile("/root/claude-changes/awg0.conf.before-enroll-"+time.Now().Format("0102-150405"), b, 0o600)
	block := fmt.Sprintf("\n# %s\n[Peer]\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = %s/32\n", inv.Name, req.Pub, psk, ip)
	f, err := os.OpenFile(awgConf, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	_, _ = f.WriteString(block)
	f.Close()
	tmp := "/run/most-admin-psk-" + randHex(4)
	_ = os.WriteFile(tmp, []byte(psk+"\n"), 0o600)
	err = exec.Command("awg", "set", "awg0", "peer", req.Pub, "preshared-key", tmp, "allowed-ips", ip+"/32").Run()
	os.Remove(tmp)
	if err != nil {
		fail(w, 500, "awg set: "+err.Error())
		return
	}
	inv.Used = true
	saveInvites(m)
	log.Printf("зарегистрировано устройство %q → %s", inv.Name, ip)
	// профиль без закрытого ключа: устройство подставит свой (PrivateKey = __LOCAL__)
	awg := fmt.Sprintf("[Interface]\nPrivateKey = __LOCAL__\nAddress = %s/24\nMTU = 1280\n%s\n[Peer]\nPublicKey = %s\nPresharedKey = %s\nEndpoint = %s\nAllowedIPs = 10.77.0.0/24\nPersistentKeepalive = 15\n",
		ip, obfLines(), srvPub, psk, cfg.Endpoint)
	prof := map[string]any{"name": cfg.Name + " (" + inv.Name + ")", "server": cfg.Server,
		"rdp_user": cfg.RDPUser, "rdp_password": cfg.RDPPassword, "xray": cfg.Xray,
		"awg": map[string]string{"conf": awg, "host": cfg.AWGHost}}
	// канал SSH: свой ключ у каждого устройства (закрытый — только на устройстве), на сервере — только проброс до RDP
	if cfg.RDPUser != "" && sshKeyRe.MatchString(req.SSH) {
		if err := addSSHKey(cfg.RDPUser, req.SSH, req.Pub); err != nil {
			log.Printf("ключ SSH устройства %q не добавлен: %v", inv.Name, err)
		} else {
			port := cfg.SSHPort
			if port == 0 {
				port = 22
			}
			prof["ssh"] = map[string]any{"user": cfg.RDPUser, "port": port, "key": "__LOCAL__"}
		}
	}
	reply(w, 200, prof)
}

// ---------- ключи SSH устройств ----------

// открытый ключ ed25519 в формате authorized_keys (без комментария)
var sshKeyRe = regexp.MustCompile(`^ssh-ed25519 AAAAC3NzaC1lZDI1NTE5[A-Za-z0-9+/]{48}$`)

// sshLimit — то же ограничение, что у ключа владельца: только проброс до рабочего стола, без командной строки
const sshLimit = `restrict,port-forwarding,permitopen="127.0.0.1:3389",command="/usr/sbin/nologin"`

var sshMu sync.Mutex

func authKeysPath(name string) (string, *user.User, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(u.HomeDir, ".ssh", "authorized_keys"), u, nil
}

// addSSHKey — дописать ключ устройства с меткой most-dev:<ключ AWG> (по ней ключ удаляется вместе с устройством).
func addSSHKey(name, key, awgPub string) error {
	sshMu.Lock()
	defer sshMu.Unlock()
	path, u, err := authKeysPath(name)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chown(dir, uid, gid)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s %s most-dev:%s\n", sshLimit, key, awgPub)
	f.Close()
	_ = os.Chown(path, uid, gid)
	return err
}

// removeSSHKey — убрать строки с меткой устройства.
func removeSSHKey(name, awgPub string) error {
	sshMu.Lock()
	defer sshMu.Unlock()
	path, _, err := authKeysPath(name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && !strings.HasSuffix(l, " most-dev:"+awgPub) {
			keep = append(keep, l)
		}
	}
	out := strings.Join(keep, "\n")
	if out != "" {
		out += "\n"
	}
	return os.WriteFile(path, []byte(out), 0o600)
}
