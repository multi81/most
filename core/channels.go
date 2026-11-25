package core

// Каналы до RDP сервера. Все работают внутри программы: без драйверов, без прав администратора, без изменения
// маршрутов Windows (поэтому не мешают INCY и другим VPN на компьютере).
//   xray — VLESS + Reality по TCP (снаружи похоже на обычный HTTPS-заход), самый стойкий к помехам;
//   awg  — AmneziaWG по UDP в пользовательском режиме (netstack);
//   ssh  — SSH-туннель напрямую, ключ на сервере умеет только проброс до 127.0.0.1:3389.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	"golang.org/x/crypto/ssh"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

type Channel interface {
	ID() string
	Title() string
	Dial(ctx context.Context) (net.Conn, error)
	Reset() // сбросить состояние после обрыва (следующий Dial поднимет канал заново)
	Close() // выключить канал совсем (смена профиля, выход)
}

// ---------- xray ----------

type xrayChannel struct {
	p    *Profile
	cdn  bool // true — VLESS+WebSocket+TLS через домен за Cloudflare
	mu   sync.Mutex
	inst *core.Instance
	emu  sync.Mutex // экземпляр для выхода в интернет (пользователь proxy)
	eg   xrayEgress
}

func (c *xrayChannel) ID() string {
	if c.cdn {
		return "cdn"
	}
	return "xray"
}

func (c *xrayChannel) Title() string {
	if c.cdn {
		return "Xray через CDN (" + c.p.Xray.WSHost + ")"
	}
	return "Xray (TCP 443, Reality)"
}

func (c *xrayChannel) start() (*core.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inst != nil {
		return c.inst, nil
	}
	x := c.p.Xray
	var cfg string
	if c.cdn {
		// адрес домена узнаём сами через DoH Cloudflare/Google — не зависим от кэша и подмены у местного DNS
		cfg = fmt.Sprintf(`{
 "log": {"loglevel": "warning"},
 "dns": {"servers": ["https+local://1.1.1.1/dns-query", "https+local://8.8.8.8/dns-query"], "queryStrategy": "UseIPv4"},
 "outbounds": [{
  "protocol": "vless",
  "settings": {"vnext": [{"address": %q, "port": %d, "users": [{"id": %q, "encryption": "none"}]}]},
  "streamSettings": {"network": "ws", "security": "tls",
   "tlsSettings": {"serverName": %q, "fingerprint": "chrome", "alpn": ["http/1.1"]},
   "wsSettings": {"path": %q, "host": %q},
   "sockopt": {"domainStrategy": "UseIPv4"}}
 }]
}`, x.WSHost, x.WSPort, x.UUID, x.WSHost, x.WSPath, x.WSHost)
	} else {
		cfg = fmt.Sprintf(`{
 "log": {"loglevel": "warning"},
 "outbounds": [{
  "protocol": "vless",
  "settings": {"vnext": [{"address": %q, "port": %d, "users": [{"id": %q, "encryption": "none", "flow": "xtls-rprx-vision"}]}]},
  "streamSettings": {"network": "tcp", "security": "reality",
   "realitySettings": {"serverName": %q, "fingerprint": "chrome", "publicKey": %q, "shortId": %q}}
 }]
}`, c.p.Server, x.Port, x.UUID, x.SNI, x.PublicKey, x.ShortID)
	}
	conf, err := serial.LoadJSONConfig(bytes.NewReader([]byte(cfg)))
	if err != nil {
		return nil, err
	}
	inst, err := core.New(conf)
	if err != nil {
		return nil, err
	}
	if err := inst.Start(); err != nil {
		return nil, err
	}
	c.inst = inst
	return inst, nil
}

func (c *xrayChannel) Dial(ctx context.Context) (net.Conn, error) {
	inst, err := c.start()
	if err != nil {
		return nil, err
	}
	// контекст не передаём: у xray соединение с сервером устанавливается в фоне уже после возврата, и отмена
	// контекста вызывающим (сразу после Dial) обрывала бы его («operation was canceled»)
	return core.Dial(context.Background(), inst, xnet.TCPDestination(xnet.ParseAddress("127.0.0.1"), xnet.Port(3389)))
}

func (c *xrayChannel) Reset() {} // xray сам переподключается на каждый Dial

func (c *xrayChannel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inst != nil {
		c.inst.Close()
	}
	c.inst = nil
	c.emu.Lock()
	if c.eg.inst != nil {
		c.eg.inst.Close()
	}
	c.eg.inst = nil
	c.emu.Unlock()
}

// ---------- AmneziaWG ----------

type awgChannel struct {
	p    *Profile
	mu   sync.Mutex
	dev  *device.Device
	tnet *netstack.Net
	hsMu sync.Mutex // ожидание рукопожатия — по одному (иначе два ожидающих пересоздавали бы туннель друг другу)
}

func (c *awgChannel) ID() string    { return "awg" }
func (c *awgChannel) Title() string { return "AmneziaWG (UDP)" }

func b64hex(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return "", errors.New("неверный ключ в конфиге AWG")
	}
	return hex.EncodeToString(b), nil
}

// awgUAPI переводит обычный конфиг AmneziaWG в формат настройки устройства (uapi) и достаёт адрес/MTU.
func awgUAPI(text string) (uapi string, addr netip.Addr, mtu int, err error) {
	mtu = 1280
	var dev, peer strings.Builder
	section := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch {
		case section == "interface" && k == "privatekey":
			h, e := b64hex(v)
			if e != nil {
				return "", addr, 0, e
			}
			fmt.Fprintf(&dev, "private_key=%s\n", h)
		case section == "interface" && k == "address":
			pfx, e := netip.ParsePrefix(strings.TrimSpace(strings.Split(v, ",")[0]))
			if e != nil {
				return "", addr, 0, e
			}
			addr = pfx.Addr()
		case section == "interface" && k == "mtu":
			mtu, _ = strconv.Atoi(v)
		case section == "interface" && (k == "jc" || k == "jmin" || k == "jmax" || k == "s1" || k == "s2" || k == "s3" || k == "s4" ||
			k == "h1" || k == "h2" || k == "h3" || k == "h4" || k == "i1" || k == "i2" || k == "i3" || k == "i4" || k == "i5"):
			fmt.Fprintf(&dev, "%s=%s\n", k, v)
		case section == "peer" && k == "publickey":
			h, e := b64hex(v)
			if e != nil {
				return "", addr, 0, e
			}
			fmt.Fprintf(&peer, "public_key=%s\n", h)
		case section == "peer" && k == "presharedkey":
			h, e := b64hex(v)
			if e != nil {
				return "", addr, 0, e
			}
			fmt.Fprintf(&peer, "preshared_key=%s\n", h)
		case section == "peer" && k == "endpoint":
			ap, e := net.ResolveUDPAddr("udp4", v)
			if e != nil {
				return "", addr, 0, e
			}
			fmt.Fprintf(&peer, "endpoint=%s\n", ap.String())
		case section == "peer" && k == "allowedips":
			// внутри программы (netstack) маршруты Windows не меняются, поэтому туннель пропускает любые адреса:
			// нужно для выхода в интернет «через наш канал»; что именно доступно, решает сервер (ufw/NAT)
			peer.WriteString("allowed_ip=0.0.0.0/0\n")
		}
	}
	if !addr.IsValid() || peer.Len() == 0 {
		return "", addr, 0, errors.New("в конфиге AWG нет адреса или сервера")
	}
	peer.WriteString("persistent_keepalive_interval=15\n")
	return dev.String() + peer.String(), addr, mtu, nil
}

func (c *awgChannel) start() (*netstack.Net, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tnet != nil {
		return c.tnet, nil
	}
	uapi, addr, mtu, err := awgUAPI(c.p.AWG.Conf)
	if err != nil {
		return nil, err
	}
	// DNS для выхода в интернет через туннель — свой unbound на сервере (10.77.0.1)
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, []netip.Addr{netip.MustParseAddr(c.p.AWG.Host)}, mtu)
	if err != nil {
		return nil, err
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "awg: "))
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, err
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, err
	}
	c.dev, c.tnet = dev, tnet
	return tnet, nil
}

// handshaked — было ли у туннеля рукопожатие с сервером (до него TCP внутри туннеля не пройдёт,
// а повторы SYN у netstack уходят в длинные паузы).
func (c *awgChannel) handshaked() bool {
	c.mu.Lock()
	dev := c.dev
	c.mu.Unlock()
	if dev == nil {
		return false
	}
	st, err := dev.IpcGet()
	if err != nil {
		return false
	}
	// только свежее: ключи сеанса WireGuard живут 180 с (при живом туннеле обновляются каждые 120 с);
	// старое рукопожатие (после сна) ничего не значит — туннель надо поднимать заново
	for _, l := range strings.Split(st, "\n") {
		if v, ok := strings.CutPrefix(l, "last_handshake_time_sec="); ok && v != "0" {
			if sec, err := strconv.ParseInt(v, 10, 64); err == nil && time.Since(time.Unix(sec, 0)) < 180*time.Second {
				return true
			}
		}
	}
	return false
}

// awgRotate — сколько ждать рукопожатия на одном UDP-порту. Замер 16.11 из офиса (10 холодных стартов):
// 4 — 0,25 с, остальные 5 с … 90+ с: по дороге режут целиком отдельные UDP-потоки (порт), повторы в тот же
// поток теряются десятками секунд, а новый порт примерно в половине случаев проходит сразу.
const awgRotate = 1200 * time.Millisecond

// ready — туннель с живым рукопожатием. Нет рукопожатия за awgRotate — пересоздать туннель (новый порт).
func (c *awgChannel) ready(ctx context.Context) (*netstack.Net, error) {
	c.hsMu.Lock()
	defer c.hsMu.Unlock()
	tnet, err := c.start()
	if err != nil {
		return nil, err
	}
	// рукопожатие туннель начинает, только когда ему есть что отправить — пинаем пустым UDP
	var lastKick time.Time
	attempt := time.Now()
	for !c.handshaked() {
		if time.Since(attempt) > awgRotate {
			c.Close()
			if tnet, err = c.start(); err != nil {
				return nil, err
			}
			attempt, lastKick = time.Now(), time.Time{}
		}
		if time.Since(lastKick) > time.Second {
			lastKick = time.Now()
			if hp, err := netip.ParseAddrPort(c.p.AWG.Host + ":9"); err == nil { // 9 — discard
				if u, err := tnet.DialUDPAddrPort(netip.AddrPort{}, hp); err == nil {
					_, _ = u.Write([]byte{0})
					u.Close()
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("нет рукопожатия с сервером по UDP")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return tnet, nil
}

func (c *awgChannel) Dial(ctx context.Context) (net.Conn, error) {
	tnet, err := c.ready(ctx)
	if err != nil {
		return nil, err
	}
	ap, err := netip.ParseAddrPort(c.p.AWG.Host + ":3389")
	if err != nil {
		return nil, err
	}
	return cleanConn(tnet.DialContextTCPAddrPort(ctx, ap))
}

// Reset для AWG ничего не рвёт: туннель сам повторяет рукопожатие, а пересоздание устройства только
// откладывает восстановление (новое рукопожатие с нуля).
func (c *awgChannel) Reset() {}

func (c *awgChannel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev != nil {
		c.dev.Close()
	}
	c.dev, c.tnet = nil, nil
}

// ---------- SSH ----------

type sshChannel struct {
	p      *Profile
	mu     sync.Mutex
	client *ssh.Client
}

func (c *sshChannel) ID() string    { return "ssh" }
func (c *sshChannel) Title() string { return "SSH напрямую (TCP 22)" }

func (c *sshChannel) get(ctx context.Context) (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	signer, err := ssh.ParsePrivateKey([]byte(c.p.SSH.Key))
	if err != nil {
		return nil, errors.New("ключ SSH не читается: " + err.Error())
	}
	cfg := &ssh.ClientConfig{User: c.p.SSH.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: tofuHostKey(c.p.Server), Timeout: 12 * time.Second}
	addr := net.JoinHostPort(c.p.Server, strconv.Itoa(c.p.SSH.Port))
	d := net.Dialer{Timeout: 12 * time.Second, KeepAlive: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	sc, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		raw.Close()
		return nil, err
	}
	cl := ssh.NewClient(sc, chans, reqs)
	go func() { // keepalive: обрыв замечаем за ~30 с, а не через минуты
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			if _, _, err := cl.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				c.Reset()
				return
			}
		}
	}()
	c.client = cl
	return cl, nil
}

func (c *sshChannel) Dial(ctx context.Context) (net.Conn, error) {
	cl, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := cl.Dial("tcp", "127.0.0.1:3389")
	if err != nil {
		c.Reset()
	}
	return conn, err
}

func (c *sshChannel) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		c.client.Close()
	}
	c.client = nil
}

func (c *sshChannel) Close() { c.Reset() }

func buildChannels(p *Profile) []Channel {
	var out []Channel
	if p.Xray != nil {
		out = append(out, &xrayChannel{p: p})
		if p.Xray.WSHost != "" {
			out = append(out, &xrayChannel{p: p, cdn: true})
		}
	}
	if p.AWG != nil {
		out = append(out, &awgChannel{p: p})
	}
	if p.SSH != nil {
		out = append(out, &sshChannel{p: p})
	}
	return out
}

// cleanConn — gvisor при ошибке отдаёт (*gonet.TCPConn)(nil): как net.Conn это не nil, и любая проверка
// «conn != nil» с последующим Close роняет программу. Отдаём настоящий nil.
func cleanConn(c *gonet.TCPConn, err error) (net.Conn, error) {
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}
