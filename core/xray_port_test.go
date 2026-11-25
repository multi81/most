package core

// Замер канала Reality до RDP (17.11): MOST_XRAY_PROFILE=<профиль .json> MOST_XRAY_PORT=443 [MOST_XRAY_N=10]
//   go test -run TestXrayPort -v ./core/

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestXrayPort(t *testing.T) {
	path := os.Getenv("MOST_XRAY_PROFILE")
	if path == "" {
		t.Skip("MOST_XRAY_PROFILE не задан")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if v, _ := strconv.Atoi(os.Getenv("MOST_XRAY_PORT")); v > 0 {
		p.Xray.Port = v
	}
	if v := os.Getenv("MOST_XRAY_SNI"); v != "" {
		p.Xray.SNI = v
	}
	n := 10
	if v, _ := strconv.Atoi(os.Getenv("MOST_XRAY_N")); v > 0 {
		n = v
	}
	ok := 0
	for i := 1; i <= n; i++ {
		c := &xrayChannel{p: &p}
		d, err := probe(c)
		if err != nil {
			t.Logf("порт %d #%d: %v", p.Xray.Port, i, err)
		} else {
			ok++
			t.Logf("порт %d #%d: RDP отвечает, %s", p.Xray.Port, i, d.Round(time.Millisecond))
		}
		c.Close()
		time.Sleep(2 * time.Second)
	}
	t.Logf("ИТОГ порт %d: %d/%d", p.Xray.Port, ok, n)
}
