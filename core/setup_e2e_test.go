package core

// Сквозная проверка мастера «Новый сервер» на чистой машине — как в программе: обследование → установка →
// профиль → каждый канал до RDP → приглашение и регистрация устройства. Долго (с рабочим столом ~45 мин).
//   MOST_SETUP_HOST=<адрес> MOST_SETUP_USER=ubuntu MOST_SETUP_KEY=<файл закрытого ключа> [MOST_SETUP_DESKTOP=1]
//   [MOST_SETUP_OUT=<куда сохранить профиль>] go test -run TestSetupE2E -v -timeout 120m ./core/

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSetupE2E(t *testing.T) {
	host := os.Getenv("MOST_SETUP_HOST")
	if host == "" {
		t.Skip("MOST_SETUP_HOST не задан")
	}
	key, err := os.ReadFile(os.Getenv("MOST_SETUP_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	desk := os.Getenv("MOST_SETUP_DESKTOP") == "1"
	req := SetupReq{Host: host, User: os.Getenv("MOST_SETUP_USER"), Key: string(key), Name: "Проверка мастера",
		Desktop: desk, Chrome: desk, AWGPort: 443}

	info, err := inspect(req)
	if err != nil {
		t.Fatal("обследование:", err)
	}
	ib, _ := json.Marshal(info)
	t.Logf("обследование: %s", ib)
	if fp, ok := info["fingerprint"].(string); ok {
		req.HostKey = fp
	}

	s := &Setup{}
	if err := s.Start(req); err != nil {
		t.Fatal(err)
	}
	seen := 0
	start := time.Now()
	for {
		time.Sleep(10 * time.Second)
		st := s.state()
		steps, _ := st["steps"].([]string)
		for ; seen < len(steps); seen++ {
			t.Logf("[%s] шаг: %s", time.Since(start).Round(time.Second), steps[seen])
		}
		if e, _ := st["error"].(string); e != "" {
			t.Logf("журнал: %s", strings.Join(st["log"].([]string), " | "))
			t.Fatalf("установка остановилась: %s", e)
		}
		if d, _ := st["done"].(bool); d {
			break
		}
		if time.Since(start) > 100*time.Minute {
			t.Fatal("установка не закончилась за 100 минут")
		}
	}
	t.Logf("установка закончена за %s", time.Since(start).Round(time.Second))
	pj, err := s.profileJSON()
	if err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("MOST_SETUP_OUT"); out != "" {
		_ = os.WriteFile(out, pj, 0o600)
	}
	var p Profile
	if err := json.Unmarshal(pj, &p); err != nil {
		t.Fatal(err)
	}
	t.Logf("профиль: %q сервер %s, rdp %v, awg %v, xray %v, ssh %v, admin %v", p.Name, p.Server,
		p.RDPUser != "", p.AWG != nil, p.Xray != nil, p.SSH != nil, p.AdminToken != "")

	if desk && p.SSH == nil {
		t.Errorf("у первого устройства нет канала SSH")
	}
	chs := buildChannels(&p)
	defer func() {
		for _, c := range chs {
			c.Close()
		}
	}()
	bad := 0
	for _, c := range chs {
		d, err := probe(c)
		if err != nil {
			bad++
			t.Errorf("канал %s: %v", c.ID(), err)
		} else {
			t.Logf("канал %s: RDP отвечает, %s", c.ID(), d.Round(time.Millisecond))
		}
	}

	var inv map[string]any
	if err := adminCall(chs, "POST", "/api/invites", p.AdminToken, map[string]string{"Name": "e2e-device"}, &inv); err != nil {
		t.Fatalf("приглашение: %v", err)
	}
	ib, _ = json.Marshal(inv)
	dp, err := enrollRaw(ib)
	if err != nil {
		t.Fatalf("регистрация по приглашению: %v", err)
	}
	t.Logf("устройство по приглашению: адрес %s", strings.Fields(strings.SplitN(strings.SplitN(dp.AWG.Conf, "Address", 2)[1], "=", 2)[1])[0])
	dc := &awgChannel{p: dp}
	if d, err := probe(dc); err != nil {
		t.Errorf("новое устройство по AWG: %v", err)
	} else {
		t.Logf("новое устройство по AWG: RDP отвечает, %s", d.Round(time.Millisecond))
	}
	dc.Close()
	if dp.SSH == nil {
		t.Errorf("новое устройство без канала SSH")
	} else if d, err := probe(&sshChannel{p: dp}); err != nil {
		t.Errorf("новое устройство по SSH: %v", err)
	} else {
		t.Logf("новое устройство по SSH: RDP отвечает, %s", d.Round(time.Millisecond))
	}
	var devs []map[string]any
	if err := adminCall(chs, "GET", "/api/devices", p.AdminToken, nil, &devs); err == nil {
		t.Logf("устройств на сервере: %d", len(devs))
	}
	err = adminCall(chs, "DELETE", "/api/devices?pub="+url.QueryEscape(awgPub(dp.AWG.Conf)), p.AdminToken, nil, &map[string]any{})
	t.Logf("устройство удалено: %v", err)
	if bad > 0 {
		t.Fatalf("не работают каналов: %d", bad)
	}
}

// TestSetupFollow — досмотреть уже идущую установку (MOST_SETUP_UNIT) после обрыва, как «Переподключиться».
func TestSetupFollow(t *testing.T) {
	unit := os.Getenv("MOST_SETUP_UNIT")
	if unit == "" {
		t.Skip("MOST_SETUP_UNIT не задан")
	}
	key, _ := os.ReadFile(os.Getenv("MOST_SETUP_KEY"))
	req := SetupReq{Host: os.Getenv("MOST_SETUP_HOST"), User: os.Getenv("MOST_SETUP_USER"), Key: string(key), Port: 22}
	info, err := inspect(req)
	if err != nil {
		t.Fatal(err)
	}
	req.HostKey, _ = info["fingerprint"].(string)
	s := &Setup{req: req, running: true, unit: unit, started: time.Now()}
	go s.follow(unit)
	seen := 0
	for start := time.Now(); time.Since(start) < 100*time.Minute; time.Sleep(10 * time.Second) {
		st := s.state()
		steps, _ := st["steps"].([]string)
		for ; seen < len(steps); seen++ {
			t.Logf("[%s] шаг: %s", time.Since(start).Round(time.Second), steps[seen])
		}
		if e, _ := st["error"].(string); e != "" {
			t.Fatalf("остановилась: %s", e)
		}
		if d, _ := st["done"].(bool); d {
			t.Logf("установка закончена (%s)", time.Since(start).Round(time.Second))
			return
		}
	}
	t.Fatal("не дождалась")
}
