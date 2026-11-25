package core

// Общее для всех платформ: адрес выхода «через наш канал» и настройки программы.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// exitIP — адрес, с которым сайты видят трафик, идущий через канал.
func (e *EgressProxy) exitIP() (string, string, error) {
	var via string
	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		h, ps, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(ps)
		c, v, err := e.dial(h, port)
		via = v
		return c, err
	}}
	cl := http.Client{Transport: tr, Timeout: 25 * time.Second}
	r, err := cl.Get("https://api.ipify.org")
	if err != nil {
		return "", via, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64))
	return strings.TrimSpace(string(b)), via, nil
}

// ----- настройки программы (что включено) -----

type Settings struct {
	Proxy bool `json:"proxy"` // прокси «через наш канал» включён
}

func settingsPath() string { return filepath.Join(appDir(), "settings.json") }

func loadSettings() Settings {
	var s Settings
	if b, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveSettings(s Settings) {
	b, _ := json.Marshal(s)
	_ = os.WriteFile(settingsPath(), b, 0o600)
}
