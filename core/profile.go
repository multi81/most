package core

// Профиль подключения: адрес сервера, учётка RDP и параметры всех каналов.
// Хранится только зашифрованным (DPAPI текущего пользователя Windows) в %APPDATA%\Most\profile.bin.
// Открытый profile.json нужен один раз — для импорта; после импорта программа предлагает его удалить.

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type SSHParams struct {
	User string `json:"user"`
	Port int    `json:"port"`
	Key  string `json:"key"` // закрытый ключ OpenSSH (ключ на сервере ограничен пробросом до 127.0.0.1:3389)
}

type AWGParams struct {
	Conf string `json:"conf"` // текст конфига AmneziaWG ([Interface]/[Peer])
	Host string `json:"host"` // адрес RDP внутри туннеля, обычно 10.77.0.1
}

type XrayParams struct {
	Port      int    `json:"port"`
	UUID      string `json:"uuid"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
	SNI       string `json:"sni"`
	// канал через CDN (Cloudflare): VLESS + WebSocket + TLS на домен; пусто — канала нет
	WSHost string `json:"ws_host,omitempty"`
	WSPort int    `json:"ws_port,omitempty"`
	WSPath string `json:"ws_path,omitempty"`
	// отдельный пользователь для выхода в интернет («через наш канал»); RDP и сам сервер ему закрыты
	ProxyUUID string `json:"proxy_uuid,omitempty"`
}

type Profile struct {
	Name        string      `json:"name"`
	Server      string      `json:"server"`
	RDPUser     string      `json:"rdp_user"`
	RDPPassword string      `json:"rdp_password"`
	SSH         *SSHParams  `json:"ssh,omitempty"`
	AWG         *AWGParams  `json:"awg,omitempty"`
	Xray        *XrayParams `json:"xray,omitempty"`
	AdminToken  string      `json:"admin_token,omitempty"` // только у устройств владельца: управление устройствами
}


// ----- несколько серверов -----
// Профили: %APPDATA%\Most\profiles\<id>.bin (каждый зашифрован DPAPI), активный — в active.txt.
// Старый одиночный profile.bin при первом запуске переносится в список.

func profilesDir() string {
	d := filepath.Join(appDir(), "profiles")
	_ = os.MkdirAll(d, 0o700)
	return d
}

var idRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// profileID — постоянный номер сервера: по адресу и имени (тот же сервер при повторной загрузке не плодит копий).
func profileID(p *Profile) string {
	h := sha256.Sum256([]byte(p.Server + "|" + p.Name))
	return hex.EncodeToString(h[:6])
}

func readEnc(path string) (*Profile, error) {
	enc, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := openSecret(enc)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func writeProfile(id string, p *Profile) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	enc, err := sealSecret(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(profilesDir(), id+".bin"), enc, 0o600)
}

func activeID() string {
	b, _ := os.ReadFile(filepath.Join(appDir(), "active.txt"))
	return strings.TrimSpace(string(b))
}

func setActive(id string) error {
	return os.WriteFile(filepath.Join(appDir(), "active.txt"), []byte(id), 0o600)
}

func migrateOld() {
	old := filepath.Join(appDir(), "profile.bin")
	p, err := readEnc(old)
	if err != nil {
		return
	}
	id := profileID(p)
	if writeProfile(id, p) == nil {
		if activeID() == "" {
			_ = setActive(id)
		}
		_ = os.Remove(old)
	}
}

// loadProfile — активный сервер.
func loadProfile() (*Profile, error) {
	migrateOld()
	id := activeID()
	if !idRe.MatchString(id) {
		return nil, errors.New("сервер не выбран")
	}
	return readEnc(filepath.Join(profilesDir(), id+".bin"))
}

type ProfileInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Server string `json:"server"`
	Active bool   `json:"active"`
	Admin  bool   `json:"admin"`
}

func listProfiles() []ProfileInfo {
	migrateOld()
	act := activeID()
	files, _ := filepath.Glob(filepath.Join(profilesDir(), "*.bin"))
	var out []ProfileInfo
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".bin")
		if !idRe.MatchString(id) {
			continue
		}
		p, err := readEnc(f)
		if err != nil {
			continue
		}
		out = append(out, ProfileInfo{ID: id, Name: p.Name, Server: p.Server, Active: id == act, Admin: p.AdminToken != ""})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func profileByID(id string) (*Profile, error) {
	if !idRe.MatchString(id) {
		return nil, errors.New("нет такого сервера")
	}
	return readEnc(filepath.Join(profilesDir(), id+".bin"))
}

func deleteProfile(id string) error {
	if !idRe.MatchString(id) {
		return errors.New("нет такого сервера")
	}
	if id == activeID() {
		return errors.New("сначала переключитесь на другой сервер")
	}
	return os.Remove(filepath.Join(profilesDir(), id+".bin"))
}

// importProfile — проверить, сохранить в список и сделать активным.
func importProfile(raw []byte) (*Profile, error) {
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("файл профиля повреждён: " + err.Error())
	}
	if p.Server == "" {
		return nil, errors.New("в профиле нет адреса сервера")
	}
	if p.SSH == nil && p.AWG == nil && p.Xray == nil {
		return nil, errors.New("в профиле нет ни одного канала")
	}
	if p.AWG != nil && p.AWG.Host == "" {
		p.AWG.Host = "10.77.0.1"
	}
	if p.SSH != nil && p.SSH.Port == 0 {
		p.SSH.Port = 22
	}
	if p.Name == "" {
		p.Name = p.Server
	}
	id := profileID(&p)
	if err := writeProfile(id, &p); err != nil {
		return nil, err
	}
	return &p, setActive(id)
}
