package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakePD — канал, который соединяется с тестовым HTTP-сервером вместо most-admin.
type fakePD struct {
	fakeRDP
	addr string
}

func (c *fakePD) DialPort(ctx context.Context, _ int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", c.addr)
}

// Обрыв посреди загрузки: первый канал (Reality) отдаёт кусок и рвёт соединение, второй докачивает по Range.
// Раньше такая загрузка считалась «повреждённой сборкой» (18.11).
func TestFetchViaResume(t *testing.T) {
	data := make([]byte, 3<<20)
	_, _ = rand.Read(data)
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "most.exe", time.Time{}, bytes.NewReader(data))
	}))
	defer full.Close()
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		_, _ = w.Write(data[:1<<20]) // треть файла — и обрыв
		hj, _ := w.(http.Hijacker)
		if c, _, err := hj.Hijack(); err == nil {
			c.Close()
		}
	}))
	defer cut.Close()
	xray := &fakePD{fakeRDP{id: "xray"}, cut.Listener.Addr().String()}
	cdn := &fakePD{fakeRDP{id: "cdn"}, full.Listener.Addr().String()}
	// cdn надёжнее xray — пойдёт первым; чтобы проверить докачку, ставим обрывающийся канал единственным «лучшим»
	cdnBad := &fakePD{fakeRDP{id: "awg"}, cut.Listener.Addr().String()}
	b, err := fetchVia([]Channel{xray, cdn, cdnBad}, "/api/update/file", int64(len(data))+1)
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	if !bytes.Equal(b, data) {
		t.Fatalf("файл собран неверно: %d байт из %d", len(b), len(data))
	}
}

// newSSHKey: открытый ключ подходит под проверку сервера (most-admin sshKeyRe), закрытый читается ssh.
func TestNewSSHKey(t *testing.T) {
	priv, pub, err := newSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^ssh-ed25519 AAAAC3NzaC1lZDI1NTE5[A-Za-z0-9+/]{48}$`).MatchString(pub) {
		t.Fatalf("открытый ключ не того вида: %q", pub)
	}
	if _, err := ssh.ParsePrivateKey([]byte(priv)); err != nil {
		t.Fatalf("закрытый ключ не читается: %v", err)
	}
}
