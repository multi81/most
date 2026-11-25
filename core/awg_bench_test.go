package core

// Замер холодного старта AWG (16.11): отдельное тестовое устройство, N раз «с нуля» (новое устройство — новый
// UDP-порт), время до рукопожатия. Запуск: MOST_AWG_BENCH=<профиль владельца .json> go test -run TestAWGColdStart -v
// В конце тестовое устройство удаляется с сервера.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
)

// awgPub — открытый ключ из строки PrivateKey конфигурации AWG.
func awgPub(conf string) string {
	for _, l := range strings.Split(conf, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "PrivateKey") {
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
			if err != nil {
				return ""
			}
			pub, _ := curve25519.X25519(b, curve25519.Basepoint)
			return base64.StdEncoding.EncodeToString(pub)
		}
	}
	return ""
}

func TestAWGColdStart(t *testing.T) {
	path := os.Getenv("MOST_AWG_BENCH")
	if path == "" {
		t.Skip("MOST_AWG_BENCH не задан")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var owner Profile
	if err := json.Unmarshal(raw, &owner); err != nil {
		t.Fatal(err)
	}
	chs := buildChannels(&owner)
	defer func() {
		for _, c := range chs {
			c.Close()
		}
	}()
	var inv map[string]any
	if err := adminCall(chs, "POST", "/api/invites", owner.AdminToken, map[string]string{"Name": "awg-bench"}, &inv); err != nil {
		t.Fatal("приглашение:", err)
	}
	ib, _ := json.Marshal(inv)
	p, err := enrollRaw(ib)
	if err != nil {
		t.Fatal("регистрация:", err)
	}
	pub := awgPub(p.AWG.Conf)
	defer func() {
		err := adminCall(chs, "DELETE", "/api/devices?pub="+url.QueryEscape(pub), owner.AdminToken, nil, &map[string]any{})
		t.Logf("тестовое устройство удалено: %v", err)
	}()
	n := 10
	if s := os.Getenv("MOST_AWG_BENCH_N"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	for i := 1; i <= n; i++ {
		c := &awgChannel{p: p}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		start := time.Now()
		conn, err := c.Dial(ctx)
		hs := time.Since(start)
		cancel()
		if err != nil {
			t.Logf("#%d: ОШИБКА за %s: %v", i, hs.Round(time.Millisecond), err)
		} else {
			conn.Close()
			t.Logf("#%d: %s", i, hs.Round(time.Millisecond))
		}
		c.Close()
		time.Sleep(3 * time.Second)
	}
}

// TestEnrollSSH — новое устройство по приглашению получает свой канал SSH до рабочего стола; после удаления
// устройства его ключ SSH на сервере не работает. MOST_AWG_BENCH=<профиль владельца> go test -run TestEnrollSSH -v
func TestEnrollSSH(t *testing.T) {
	path := os.Getenv("MOST_AWG_BENCH")
	if path == "" {
		t.Skip("MOST_AWG_BENCH не задан")
	}
	raw, _ := os.ReadFile(path)
	var owner Profile
	if err := json.Unmarshal(raw, &owner); err != nil {
		t.Fatal(err)
	}
	chs := buildChannels(&owner)
	defer func() {
		for _, c := range chs {
			c.Close()
		}
	}()
	var inv map[string]any
	if err := adminCall(chs, "POST", "/api/invites", owner.AdminToken, map[string]string{"Name": "ssh-test"}, &inv); err != nil {
		t.Fatal("приглашение:", err)
	}
	ib, _ := json.Marshal(inv)
	p, err := enrollRaw(ib)
	if err != nil {
		t.Fatal("регистрация:", err)
	}
	pub := awgPub(p.AWG.Conf)
	if p.SSH == nil {
		_ = adminCall(chs, "DELETE", "/api/devices?pub="+url.QueryEscape(pub), owner.AdminToken, nil, &map[string]any{})
		t.Fatal("сервер не выдал канал SSH")
	}
	d, err := probe(&sshChannel{p: p})
	if err != nil {
		t.Errorf("SSH до рабочего стола: %v", err)
	} else {
		t.Logf("SSH нового устройства: RDP отвечает, %s", d.Round(time.Millisecond))
	}
	if err := adminCall(chs, "DELETE", "/api/devices?pub="+url.QueryEscape(pub), owner.AdminToken, nil, &map[string]any{}); err != nil {
		t.Fatal("удаление:", err)
	}
	if _, err := probe(&sshChannel{p: p}); err == nil {
		t.Error("ключ SSH удалённого устройства всё ещё пускает")
	} else {
		t.Logf("после удаления устройства SSH не пускает: %v", err)
	}
}
