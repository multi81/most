package core

// Устройства: управление через службу most-admin на сервере (127.0.0.1:8787 / 10.77.0.1:8787, только изнутри
// каналов) и регистрация нового устройства по приглашению. Ключ AWG нового устройства создаётся здесь,
// на сервер уходит только открытая часть.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ssh"
)

const adminPort = 8787

// PortDialer — канал, умеющий дойти до любого локального порта сервера (не только RDP).
type PortDialer interface {
	Channel
	DialPort(ctx context.Context, port int) (net.Conn, error)
}

func (c *xrayChannel) DialPort(_ context.Context, port int) (net.Conn, error) {
	inst, err := c.start()
	if err != nil {
		return nil, err
	}
	return core.Dial(context.Background(), inst, xnet.TCPDestination(xnet.ParseAddress("127.0.0.1"), xnet.Port(port)))
}

func (c *awgChannel) DialPort(ctx context.Context, port int) (net.Conn, error) {
	tnet, err := c.ready(ctx)
	if err != nil {
		return nil, err
	}
	return cleanConn(tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(netip.MustParseAddr(c.p.AWG.Host), uint16(port))))
}

// adminCall — запрос к most-admin через первый живой канал (порядок: AWG, CDN, Reality).
func adminCall(chs []Channel, method, path, token string, body any, out any) error {
	var lastErr error = errors.New("нет живого канала до сервера")
	rank := map[string]int{"awg": 0, "cdn": 1, "xray": 2}
	var list []PortDialer
	for _, c := range chs {
		if pd, ok := c.(PortDialer); ok {
			list = append(list, pd)
		}
	}
	for i := 0; i < len(list); i++ { // простая сортировка по надёжности
		for j := i + 1; j < len(list); j++ {
			if rank[list[j].ID()] < rank[list[i].ID()] {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	for _, ch := range list {
		ch := ch
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return ch.DialPort(ctx, adminPort) }}
		cl := http.Client{Transport: tr, Timeout: 30 * time.Second}
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, "http://most-admin"+path, rd)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Admin", token)
		}
		resp, err := cl.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", ch.ID(), err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			var e struct{ Error string }
			_ = json.Unmarshal(b, &e)
			if e.Error == "" {
				e.Error = resp.Status
			}
			return errors.New(e.Error) // сервер ответил — другие каналы пробовать незачем
		}
		return json.Unmarshal(b, out)
	}
	return lastErr
}

// newAWGKey — пара ключей AWG (Curve25519): закрытый остаётся на устройстве.
func newAWGKey() (priv, pub string) {
	var k [32]byte
	_, _ = rand.Read(k[:])
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	p, _ := curve25519.X25519(k[:], curve25519.Basepoint)
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(p)
}

type invite struct {
	Kind   string      `json:"kind"`
	Name   string      `json:"name"`
	Device string      `json:"device"`
	Code   string      `json:"code"`
	Server string      `json:"server"`
	Xray   *XrayParams `json:"xray"`
}

// enrollByInvite — новое устройство: дойти до сервера каналами Xray из приглашения, зарегистрировать свой
// открытый ключ AWG, получить полный профиль и подставить в него закрытый ключ.
func enrollByInvite(raw []byte) (*Profile, error) {
	p, err := enrollRaw(raw)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(p)
	return importProfile(b)
}

// enrollRaw — регистрация без сохранения профиля (для самопроверки).
func enrollRaw(raw []byte) (*Profile, error) {
	var inv invite
	if err := json.Unmarshal(raw, &inv); err != nil || inv.Kind != "most-invite" || inv.Code == "" || inv.Xray == nil {
		return nil, errors.New("это не приглашение «Моста»")
	}
	tmp := &Profile{Server: inv.Server, Xray: inv.Xray}
	chs := buildChannels(tmp)
	defer func() {
		for _, c := range chs {
			c.Close()
		}
	}()
	priv, pub := newAWGKey()
	sshPriv, sshPub, _ := newSSHKey() // свой ключ для канала SSH; закрытый остаётся на устройстве
	var p Profile
	if err := adminCall(chs, "POST", "/api/enroll", "", map[string]string{"code": inv.Code, "pub": pub, "ssh": sshPub}, &p); err != nil {
		return nil, errors.New("сервер не принял приглашение: " + err.Error())
	}
	if p.AWG == nil || !strings.Contains(p.AWG.Conf, "__LOCAL__") {
		return nil, errors.New("сервер вернул неполный профиль")
	}
	p.AWG.Conf = strings.Replace(p.AWG.Conf, "__LOCAL__", priv, 1)
	if p.SSH != nil { // сервер добавил наш ключ SSH (старый сервер канала SSH не даёт — профиль без него)
		if p.SSH.Key != "__LOCAL__" || sshPriv == "" {
			p.SSH = nil
		} else {
			p.SSH.Key = sshPriv
		}
	}
	return &p, nil
}

// newSSHKey — пара ключей ed25519 для канала SSH: закрытый (OpenSSH PEM) и открытый (строка authorized_keys).
func newSSHKey() (priv, pub string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	blk, err := ssh.MarshalPrivateKey(sk, "")
	if err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(blk)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))), nil
}
