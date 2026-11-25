package core

// «Через наш канал»: локальный прокси 127.0.0.1:10870 (SOCKS5) и 127.0.0.1:10871 (HTTP, в т.ч. CONNECT).
// Каждое соединение уходит через лучший живой канал, умеющий выход в интернет: AWG (сервер делает NAT, DNS — свой
// unbound 10.77.0.1), Xray через CDN или Xray Reality (отдельный пользователь «proxy» — ему закрыты RDP и сам сервер).
// Адреса по именам разрешаются на сервере (утечек DNS мимо канала нет). UDP не поддерживается — браузеры
// в этом случае сами переходят с QUIC на обычный TCP.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
)

const socksPort, httpPort = 10870, 10871

// Egresser — канал, через который можно выйти в интернет.
type Egresser interface {
	Channel
	DialOut(ctx context.Context, host string, port int) (net.Conn, error)
}

// ----- AWG: прямо в туннель (сервер выпускает 10.77.0.0/24 в интернет) -----

func (c *awgChannel) DialOut(ctx context.Context, host string, port int) (net.Conn, error) {
	tnet, err := c.ready(ctx)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil { // имя — спросить свой DNS на сервере и взять адрес IPv4 (туннель только IPv4)
		addrs, lerr := tnet.LookupContextHost(ctx, host)
		if lerr != nil {
			return nil, lerr
		}
		for _, a := range addrs {
			if x, e := netip.ParseAddr(a); e == nil && x.Is4() {
				ip = x
				break
			}
		}
		if !ip.IsValid() {
			return nil, errors.New("у " + host + " нет адреса IPv4")
		}
	}
	return cleanConn(tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(ip, uint16(port))))
}

// ----- Xray: отдельный экземпляр с пользователем proxy -----

type xrayEgress struct {
	mu   sync.Mutex
	inst *core.Instance
}

func (c *xrayChannel) egressInst() (*core.Instance, error) {
	c.emu.Lock()
	defer c.emu.Unlock()
	if c.eg.inst != nil {
		return c.eg.inst, nil
	}
	x := c.p.Xray
	if x.ProxyUUID == "" {
		return nil, errors.New("в профиле нет пользователя для выхода в интернет — обновите профиль")
	}
	var cfg string
	if c.cdn {
		cfg = fmt.Sprintf(`{"log":{"loglevel":%q},
 "dns":{"servers":["https+local://1.1.1.1/dns-query","https+local://8.8.8.8/dns-query"],"queryStrategy":"UseIPv4"},
 "outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":%d,"users":[{"id":%q,"encryption":"none"}]}]},
  "streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":%q,"fingerprint":"chrome","alpn":["http/1.1"]},
   "wsSettings":{"path":%q,"host":%q},"sockopt":{"domainStrategy":"UseIPv4"}}}]}`,
			xrayLogLevel(), x.WSHost, x.WSPort, x.ProxyUUID, x.WSHost, x.WSPath, x.WSHost)
	} else {
		cfg = fmt.Sprintf(`{"log":{"loglevel":"warning"},
 "outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":%q,"port":%d,"users":[{"id":%q,"encryption":"none","flow":"xtls-rprx-vision"}]}]},
  "streamSettings":{"network":"tcp","security":"reality","realitySettings":{"serverName":%q,"fingerprint":"chrome","publicKey":%q,"shortId":%q}}}]}`,
			c.p.Server, x.Port, x.ProxyUUID, x.SNI, x.PublicKey, x.ShortID)
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
	c.eg.inst = inst
	return inst, nil
}

func (c *xrayChannel) DialOut(ctx context.Context, host string, port int) (net.Conn, error) {
	inst, err := c.egressInst()
	if err != nil {
		return nil, err
	}
	return core.Dial(context.Background(), inst, // см. xrayChannel.Dial: контекст вызывающего не передаём
		 xnet.TCPDestination(xnet.ParseAddress(host), xnet.Port(port)))
}

// ----- выбор канала и сам прокси -----

type EgressProxy struct {
	mu      sync.Mutex
	f       *Forwarder
	lns     []net.Listener
	on      bool
	conns   int
	lastVia string
}

// candidates — живые каналы с выходом в интернет в порядке надёжности: AWG (ошибка видна сразу при соединении),
// затем Xray через CDN, затем Xray Reality (у xray сбой связи виден только при первом чтении — переключиться уже нельзя).
func (e *EgressProxy) candidates() []Egresser {
	rank := map[string]int{"awg": 0, "cdn": 1, "xray": 2}
	var out []Egresser
	for _, c := range e.f.usable() {
		if eg, ok := c.(Egresser); ok {
			out = append(out, eg)
		}
	}
	e.f.mu.Lock()
	mode := e.f.mode
	e.f.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].ID() == mode) != (out[j].ID() == mode) { // канал, выбранный вручную, — первым
			return out[i].ID() == mode
		}
		return rank[out[i].ID()] < rank[out[j].ID()]
	})
	return out
}

func (e *EgressProxy) dial(host string, port int) (net.Conn, string, error) {
	var last error = errors.New("нет живого канала с выходом в интернет")
	for _, ch := range e.candidates() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		c, err := ch.DialOut(ctx, host, port)
		cancel()
		if err == nil {
			return countConn{Conn: c, c: e.f.stats.ch(ch.ID())}, ch.ID(), nil
		}
		log.Printf("выход через %s до %s:%d не удался: %v", ch.ID(), host, port, err)
		last = err
	}
	return nil, "", last
}

func (e *EgressProxy) Start(f *Forwarder) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.on {
		return nil
	}
	e.f = f
	s, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(socksPort))
	if err != nil {
		return fmt.Errorf("порт %d занят: %w", socksPort, err)
	}
	h, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(httpPort))
	if err != nil {
		s.Close()
		return fmt.Errorf("порт %d занят: %w", httpPort, err)
	}
	e.lns, e.on = []net.Listener{s, h}, true
	go e.serve(s, e.handleSOCKS)
	go e.serve(h, e.handleHTTP)
	log.Printf("прокси «через наш канал» включён: SOCKS5 %d, HTTP %d", socksPort, httpPort)
	return nil
}

func (e *EgressProxy) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.lns {
		l.Close()
	}
	e.lns, e.on = nil, false
	log.Printf("прокси «через наш канал» выключен")
}

func (e *EgressProxy) state() (bool, int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.on, e.conns, e.lastVia
}

func (e *EgressProxy) serve(ln net.Listener, h func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			e.mu.Lock()
			e.conns++
			e.mu.Unlock()
			h(c)
			e.mu.Lock()
			e.conns--
			e.mu.Unlock()
		}()
	}
}

// relay: соединиться с адресом через канал, вызвать ready (ответ клиенту или начало запроса серверу), дальше — труба.
func (e *EgressProxy) relay(local net.Conn, host string, port int, ready func(remote net.Conn) error) {
	remote, via, err := e.dial(host, port)
	if err != nil {
		local.Close()
		return
	}
	e.mu.Lock()
	e.lastVia = via
	e.mu.Unlock()
	if err := ready(remote); err != nil {
		remote.Close()
		local.Close()
		return
	}
	pipe(local, remote)
}

// SOCKS5 (RFC 1928), только CONNECT, без пароля (слушает только 127.0.0.1).
func (e *EgressProxy) handleSOCKS(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(c)
	h := make([]byte, 2)
	if _, err := io.ReadFull(br, h); err != nil || h[0] != 5 {
		c.Close()
		return
	}
	if _, err := io.ReadFull(br, make([]byte, h[1])); err != nil {
		c.Close()
		return
	}
	c.Write([]byte{5, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[1] != 1 {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		c.Close()
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(br, b)
		host = netip.AddrFrom4([4]byte(b)).String()
	case 3:
		l, _ := br.ReadByte()
		b := make([]byte, l)
		io.ReadFull(br, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		io.ReadFull(br, b)
		host = netip.AddrFrom16([16]byte(b)).String()
	default:
		c.Close()
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		c.Close()
		return
	}
	port := int(binary.BigEndian.Uint16(pb))
	_ = c.SetDeadline(time.Time{})
	e.relay(bufConn{c, br}, host, port, func(net.Conn) error {
		_, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		return err
	})
}

// HTTP-прокси: CONNECT (HTTPS) и обычные http:// запросы.
func (e *EgressProxy) handleHTTP(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	if req.Method == http.MethodConnect {
		host, ps, err := net.SplitHostPort(req.Host)
		if err != nil {
			c.Close()
			return
		}
		port, _ := strconv.Atoi(ps)
		e.relay(bufConn{c, br}, host, port, func(net.Conn) error {
			_, err := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			return err
		})
		return
	}
	host, port := req.URL.Hostname(), 80
	if p := req.URL.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	if host == "" {
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		c.Close()
		return
	}
	req.RequestURI = ""
	req.Header.Del("Proxy-Connection")
	req.Header.Set("Connection", "close") // одно соединение — один запрос (проще и надёжно)
	e.relay(bufConn{c, br}, host, port, func(remote net.Conn) error {
		return req.Write(remote) // запрос в виде для сервера (путь без хоста) вместе с телом
	})
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}


func xrayLogLevel() string {
	if os.Getenv("MOST_DEBUG") != "" {
		return "debug"
	}
	return "warning"
}
