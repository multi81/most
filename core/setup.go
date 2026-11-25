package core

// Мастер подготовки нового сервера: SSH (пароль или ключ) → отпечаток ключа сервера на подтверждение → обследование →
// установка (setup/server-setup.sh запускается на сервере отдельной службой, журнал читается с переподключением) →
// профиль. Ключ AWG этого устройства создаётся здесь, на сервер уходит только открытая часть.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"regexp"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

//go:embed setup/server-setup.sh setup/most-admin.go.txt
var setupFS embed.FS

type SetupReq struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Key      string `json:"key"`        // закрытый ключ OpenSSH (текст), если вход по ключу
	HostKey  string `json:"hostkey"`    // подтверждённый отпечаток (SHA256:…)
	Name     string `json:"name"`       // название сервера в программе
	Desktop  bool   `json:"desktop"`    // ставить рабочий стол KDE + RDP
	AWGPort  int    `json:"awg_port"`   // UDP-порт AmneziaWG
	Domain   string `json:"domain"`     // по желанию: домен в Cloudflare для канала «Xray через CDN»
	RealityDomain string `json:"reality_domain"` // по желанию: свой домен (A → сервер, без облака) — маскировка Reality под свой сайт
	Chrome   bool   `json:"chrome"`     // поставить Google Chrome на рабочий стол сервера
	Claude   bool   `json:"claude"`     // поставить Claude Desktop (официальная бета Anthropic для Linux)
}

type Setup struct {
	mu      sync.Mutex
	req     SetupReq
	running bool
	steps   []string
	logTail []string
	errMsg  string
	done    bool
	result  *Profile
	priv    string
	sshPriv string // закрытый ключ SSH первого устройства (в профиль после установки)
	unit    string
	started time.Time
}

// Reset — забыть законченную установку: шаги, журнал, профиль и ключи (они уже в программе или не нужны).
func (s *Setup) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	s.steps, s.logTail, s.errMsg, s.done, s.result, s.priv, s.sshPriv, s.unit = nil, nil, "", false, nil, "", "", ""
	s.req = SetupReq{}
}

func (s *Setup) state() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := map[string]any{"running": s.running, "steps": s.steps, "log": s.logTail, "error": s.errMsg, "done": s.done}
	if s.result != nil {
		st["profile_name"] = s.result.Name
	}
	return st
}

func fingerprint(k ssh.PublicKey) string {
	h := sha256.Sum256(k.Marshal())
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(h[:]), "=")
}

func (r *SetupReq) dial(accept func(fp string) error) (*ssh.Client, error) {
	var auth []ssh.AuthMethod
	if strings.TrimSpace(r.Key) != "" {
		signer, err := ssh.ParsePrivateKey([]byte(r.Key))
		if err != nil {
			return nil, errors.New("ключ не читается (нужен закрытый ключ OpenSSH без пароля): " + err.Error())
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if r.Password != "" {
		auth = append(auth, ssh.Password(r.Password), ssh.KeyboardInteractive(func(_, _ string, q []string, _ []bool) ([]string, error) {
			a := make([]string, len(q))
			for i := range a {
				a[i] = r.Password
			}
			return a, nil
		}))
	}
	if len(auth) == 0 {
		return nil, errors.New("укажите пароль или ключ")
	}
	if r.Port == 0 {
		r.Port = 22
	}
	cfg := &ssh.ClientConfig{User: r.User, Auth: auth, Timeout: 20 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error { return accept(fingerprint(k)) }}
	return ssh.Dial("tcp", net.JoinHostPort(r.Host, strconv.Itoa(r.Port)), cfg)
}

// sudo — команда от root: как есть для root, иначе через sudo (с паролем, если вход по паролю).
func (r *SetupReq) run(c *ssh.Client, cmd string, stdin []byte) (string, error) {
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	full := cmd
	in := stdin
	if r.User != "root" {
		if r.Password != "" {
			full = "sudo -k -S -p '' bash -c " + shq(cmd) // -k: пароль читается всегда, иначе он попал бы в данные команды
			in = append([]byte(r.Password+"\n"), stdin...)
		} else {
			full = "sudo -n bash -c " + shq(cmd)
		}
	} else {
		full = "bash -c " + shq(cmd)
	}
	if in != nil {
		sess.Stdin = bytes.NewReader(in)
	}
	var out bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &out
	err = sess.Run(full)
	return out.String(), err
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// inspect — подключение и обследование; возвращает отпечаток ключа сервера (для подтверждения) и сведения.
func inspect(r SetupReq) (map[string]any, error) {
	var fp string
	c, err := r.dial(func(f string) error { fp = f; return nil })
	if err != nil {
		if fp != "" && strings.Contains(err.Error(), "unable to authenticate") {
			return nil, errors.New("сервер не принял логин, пароль или ключ")
		}
		return nil, errors.New("не удалось подключиться: " + err.Error())
	}
	defer c.Close()
	const probe = `. /etc/os-release; echo "os=$PRETTY_NAME"; echo "ver=$VERSION_ID"; echo "arch=$(uname -m)";
echo "cpu=$(nproc)"; echo "ram=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)"; echo "disk=$(df -BG --output=avail / | tail -1 | tr -d ' G')";
echo "virt=$(systemd-detect-virt 2>/dev/null)"; echo "kvm=$([ -e /dev/kvm ] && echo да || echo нет)";
echo "busy=$(ss -Htuln 2>/dev/null | awk '{print $5}' | sed 's/.*://' | sort -un | tr '\n' ' ')";
echo "prev=$([ -s /etc/most-admin/config.json ] && echo да || echo нет)"`
	// под нагрузкой (идёт установка, обновления) ответ бывает пустым без ошибки — повторить,
	// а не показывать ложные «не та система» (проверка мастера 17.11)
	info := map[string]any{"fingerprint": fp}
	for try := 1; ; try++ {
		out, err := r.run(c, probe, nil)
		if err != nil {
			return nil, errors.New("обследование не прошло (нет прав root/sudo?): " + strings.TrimSpace(out))
		}
		for _, l := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				info[k] = v
			}
		}
		if info["ver"] != nil && info["arch"] != nil {
			break
		}
		if try == 3 {
			return nil, errors.New("сервер не ответил на обследование (пустой ответ) — подождите минуту и нажмите «Проверить сервер» ещё раз")
		}
		time.Sleep(3 * time.Second)
	}
	var warn []string
	if v, _ := info["ver"].(string); v != "24.04" && v != "26.04" {
		warn = append(warn, "поддерживаются только Ubuntu 24.04 и 26.04")
	}
	if a, _ := info["arch"].(string); a != "x86_64" {
		warn = append(warn, "нужна архитектура x86_64")
	}
	if ram, _ := strconv.Atoi(fmt.Sprint(info["ram"])); ram > 0 && ram < 1800 {
		warn = append(warn, "мало памяти: для рабочего стола нужно от 4 ГБ, без него — от 2 ГБ")
	}
	if d, _ := strconv.Atoi(fmt.Sprint(info["disk"])); d > 0 && d < 10 {
		warn = append(warn, "мало места на диске (нужно от 10 ГБ, с рабочим столом — от 20 ГБ)")
	}
	info["warnings"] = warn
	return info, nil
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

func randPassword(n int) string {
	const abc = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		x, _ := rand.Int(rand.Reader, big.NewInt(int64(len(abc))))
		b[i] = abc[x.Int64()]
	}
	return string(b)
}

func (s *Setup) Start(r SetupReq) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("подготовка уже идёт")
	}
	if r.AWGPort == 0 {
		r.AWGPort = 443
	}
	r.Domain = strings.ToLower(strings.TrimSpace(r.Domain))
	if r.Domain != "" && !domainRe.MatchString(r.Domain) {
		s.mu.Unlock()
		return errors.New("домен записан неверно (пример: vpn.example.com)")
	}
	r.RealityDomain = strings.ToLower(strings.TrimSpace(r.RealityDomain))
	if r.RealityDomain != "" && !domainRe.MatchString(r.RealityDomain) {
		s.mu.Unlock()
		return errors.New("домен для маскировки записан неверно (пример: site.example.com)")
	}
	if r.RealityDomain != "" && r.RealityDomain == r.Domain {
		s.mu.Unlock()
		return errors.New("домен для маскировки и домен для CDN должны быть разными: первый — без облака Cloudflare, второй — с облаком")
	}
	s.req, s.running, s.started = r, true, time.Now()
	s.steps, s.logTail, s.errMsg, s.done, s.result, s.priv, s.sshPriv, s.unit = nil, nil, "", false, nil, "", "", ""
	s.mu.Unlock()
	go s.work()
	return nil
}

// Reattach — снова следить за уже запущенной установкой (после обрыва связи); ключ устройства хранится в памяти.
func (s *Setup) Reattach() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	if s.unit == "" || s.priv == "" {
		return errors.New("нечего продолжать — начните подготовку заново (это безопасно)")
	}
	s.running, s.errMsg = true, ""
	go s.follow(s.unit)
	return nil
}

func (s *Setup) fail(msg string) {
	s.mu.Lock()
	s.errMsg, s.running = msg, false
	s.mu.Unlock()
}

func (s *Setup) connect() (*ssh.Client, error) {
	r := s.req
	return r.dial(func(fp string) error {
		if fp != r.HostKey {
			return errors.New("ключ сервера не совпадает с подтверждённым — подключение остановлено")
		}
		return nil
	})
}

func (s *Setup) work() {
	r := s.req
	c, err := s.connect()
	if err != nil {
		s.fail("не удалось подключиться: " + err.Error())
		return
	}
	priv, pub := newAWGKey()
	sshPriv, sshPub, _ := newSSHKey() // канал SSH первого устройства: закрытый ключ остаётся здесь
	s.mu.Lock()
	s.priv, s.sshPriv = priv, sshPriv
	s.mu.Unlock()
	script, _ := setupFS.ReadFile("setup/server-setup.sh")
	admin, _ := setupFS.ReadFile("setup/most-admin.go.txt")
	pw := randPassword(20)
	if out, err := r.run(c, "install -d -m700 /root/most-setup && cat > /root/most-setup/server-setup.sh", script); err != nil {
		s.fail("не удалось передать скрипт: " + out)
		return
	}
	if out, err := r.run(c, "cat > /root/most-setup/most-admin.go", admin); err != nil {
		s.fail("не удалось передать файлы: " + out)
		return
	}
	if out, err := r.run(c, "umask 077; cat > /root/most-setup/pw", []byte(pw+"\n")); err != nil {
		s.fail("не удалось передать пароль: " + out)
		return
	}
	desk := "0"
	if r.Desktop {
		desk = "1"
	}
	unit := "most-setup-" + strconv.FormatInt(time.Now().Unix(), 10)
	cmd := fmt.Sprintf("systemd-run --unit=%s --collect --setenv=MOST_OWNER_PUB=%s --setenv=MOST_AWG_PORT=%d --setenv=MOST_DESKTOP=%s "+
		"--setenv=MOST_PWFILE=/root/most-setup/pw --setenv=MOST_PUBLIC_IP=%s --setenv=MOST_DOMAIN=%s --setenv=MOST_REALITY_DOMAIN=%s --setenv=MOST_CHROME=%s --setenv=MOST_CLAUDE=%s --setenv=MOST_OWNER_SSH=%s --setenv=MOST_SRC=/root/most-setup bash /root/most-setup/server-setup.sh",
		unit, shq(pub), r.AWGPort, desk, shq(r.Host), shq(r.Domain), shq(r.RealityDomain), map[bool]string{true: "1", false: "0"}[r.Chrome && r.Desktop], map[bool]string{true: "1", false: "0"}[r.Claude && r.Desktop], shq(sshPub))
	if out, err := r.run(c, ": > /var/log/most-setup.log; "+cmd, nil); err != nil {
		s.fail("не удалось запустить установку: " + out)
		return
	}
	c.Close()
	s.mu.Lock()
	s.unit = unit
	s.mu.Unlock()
	s.follow(unit)
}

// follow — читает журнал установки; при обрыве SSH переподключается (установка на сервере идёт сама).
func (s *Setup) follow(unit string) {
	r := s.req
	off, idle, lastErr := 0, 0, ""
	var c *ssh.Client
	// установка на сервере идёт сама: связь может пропадать (перезапуск сети, тяжёлая сборка, сон компьютера) —
	// сдаёмся только через 20 минут без единого удачного опроса
	lastOK := time.Now()
	for fails := 0; time.Since(lastOK) < 20*time.Minute; {
		if c == nil {
			var err error
			if c, err = s.connect(); err != nil {
				if fails%10 == 0 {
					log.Printf("мастер: нет подключения к серверу (%d): %v", fails, err)
				}
				fails++
				time.Sleep(5 * time.Second)
				continue
			}
		}
		out, err := r.run(c, fmt.Sprintf("tail -c +%d /var/log/most-setup.log; echo; echo \"##STATE $(systemctl is-active %s 2>/dev/null)\"", off+1, unit), nil)
		if err != nil && !strings.Contains(out, "##STATE") {
			log.Printf("мастер: опрос не прошёл: %v %q", err, strings.TrimSpace(out))
			c.Close()
			c = nil
			fails++
			time.Sleep(3 * time.Second)
			continue
		}
		fails = 0
		lastOK = time.Now()
		body, stateLine, found := strings.Cut(out, "\n##STATE ")
		if !found { // пустой или обрезанный ответ (бывает под тяжёлой установкой) — переподключиться и спросить снова
			c.Close()
			c = nil
			fails++
			time.Sleep(3 * time.Second)
			continue
		}
		off += len(body) // перевод строки перед ##STATE — от нашего echo, в body его нет
		state := strings.TrimSpace(stateLine)
		done, errMsg := false, ""
		s.mu.Lock()
		for _, l := range strings.Split(body, "\n") {
			switch {
			case strings.HasPrefix(l, "##STEP "):
				s.steps = append(s.steps, strings.TrimPrefix(l, "##STEP "))
			case strings.HasPrefix(l, "##ERR "):
				errMsg = strings.TrimPrefix(l, "##ERR ")
			case l == "##DONE":
				done = true
			case strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "##"):
				s.logTail = append(s.logTail, l)
				if len(s.logTail) > 200 {
					s.logTail = s.logTail[len(s.logTail)-200:]
				}
			}
		}
		s.mu.Unlock()
		if errMsg != "" {
			lastErr = errMsg // строка ##ERR могла прийти, пока служба ещё числилась работающей
		}
		if lastErr != "" && state != "active" && state != "activating" {
			s.fail(lastErr)
			c.Close()
			return
		}
		if done {
			s.finish(c)
			c.Close()
			return
		}
		if state == "active" || state == "activating" || body != "" {
			idle = 0
		} else if idle++; idle >= 3 { // три опроса подряд служба не работает и журнал не растёт
			log.Printf("мастер: опрос без состояния: err=%v out=%q", err, out)
			s.fail("установка на сервере остановилась без итога (состояние службы: «" + state + "») — см. журнал")
			c.Close()
			return
		}
		time.Sleep(2 * time.Second)
	}
	s.fail("связь с сервером потеряна надолго; установка, возможно, продолжается — попробуйте ещё раз позже")
}

func (s *Setup) finish(c *ssh.Client) {
	r := s.req
	out, err := r.run(c, "cat /root/most-setup-result.json && shred -u /root/most-setup-result.json", nil)
	if err != nil {
		s.fail("не удалось забрать итог: " + out)
		return
	}
	// в выводе бывают и предупреждения sudo (stderr вместе со stdout: «unable to resolve host» сразу после
	// перезапуска служб) — взять только сам JSON
	js := strings.TrimSpace(out)
	if i, j := strings.Index(js, "{"), strings.LastIndex(js, "}"); i >= 0 && j > i {
		js = js[i : j+1]
	}
	var p Profile
	if err := json.Unmarshal([]byte(js), &p); err != nil || p.AWG == nil {
		why := "нет AWG"
		if err != nil {
			why = err.Error() // текст ошибки разбора, без самого итога (в нём пароль)
		}
		log.Printf("мастер: итог не разобран (%d байт): %s", len(out), why)
		s.fail("итог установки повреждён: " + why)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p.AWG.Conf = strings.Replace(p.AWG.Conf, "__LOCAL__", s.priv, 1)
	if p.SSH != nil { // сервер добавил наш ключ SSH (только с рабочим столом)
		if p.SSH.Key == "__LOCAL__" && s.sshPriv != "" {
			p.SSH.Key = s.sshPriv
		} else {
			p.SSH = nil
		}
	}
	if r.Name != "" {
		p.Name = r.Name
	}
	s.result, s.done, s.running = &p, true, false
}

func (s *Setup) profileJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.result == nil {
		return nil, errors.New("подготовка ещё не закончена")
	}
	return json.MarshalIndent(s.result, "", " ")
}
