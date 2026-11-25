package core

// Локальный вход 127.0.0.1:<порт> для mstsc. Каждое новое RDP-соединение уходит в лучший живой канал;
// при обрыве mstsc сам переподключается, и новое соединение берёт следующий живой канал.
// Проверка каналов: раз в 30 с каждый канал доходит до RDP сервера и получает ответ на приветствие X.224.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type ChannelState struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	OK      bool    `json:"ok"`
	Ms      float64 `json:"ms"`
	Err     string  `json:"err,omitempty"`
	Checked string  `json:"checked"`
	Active  int     `json:"active"` // сколько RDP-соединений идёт через канал сейчас
	Drops   int     `json:"drops"`  // обрывов посреди сеанса за время работы программы
	Penalty bool    `json:"penalty"` // после обрыва канал временно в конце очереди
}

const dropPenalty = 5 * time.Minute // сколько оборвавшийся канал стоит в конце очереди

type Forwarder struct {
	mu       sync.Mutex
	channels []Channel
	state    map[string]*ChannelState
	mode     string // "auto" или ID канала
	port     int
	ln       net.Listener
	stats    *Stats
	live     map[net.Conn]struct{} // открытые RDP-соединения (для «Отключиться»)
	stopped  bool
	kick     map[string]chan struct{} // «проверь сейчас» для цикла проверки канала (после сна, обрыва)
	penalty  map[string]time.Time // до какого времени канал в конце очереди (после обрыва)
	lastDrop time.Time            // последний обрыв посреди сеанса
}

// penalized — канал недавно оборвался посреди сеанса (вызывать под f.mu).
func (f *Forwarder) penalized(id string) bool { return time.Now().Before(f.penalty[id]) }

// LastDropWithin — был ли обрыв посреди сеанса за последние d (для автооткрытия окна RDP).
func (f *Forwarder) LastDropWithin(d time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.lastDrop.IsZero() && time.Since(f.lastDrop) < d
}

// markDrop — соединение оборвалось со стороны сервера: канал упал, на 5 минут в конец очереди, перепроверить все.
func (f *Forwarder) markDrop(ch Channel, lasted time.Duration) {
	f.mu.Lock()
	s := f.state[ch.ID()]
	s.OK, s.Err = false, "оборвался посреди сеанса — проверяю заново"
	s.Drops++
	f.penalty[ch.ID()] = time.Now().Add(dropPenalty)
	f.lastDrop = time.Now()
	f.mu.Unlock()
	log.Printf("обрыв через %s спустя %s — канал на %v в конец очереди, переключаюсь", ch.ID(), lasted.Round(time.Second), dropPenalty)
	ch.Reset()
	f.kickAll()
}

func newForwarder(chs []Channel, port int) *Forwarder {
	f := &Forwarder{channels: chs, state: map[string]*ChannelState{}, mode: "auto", port: port,
		stats: newStats(), live: map[net.Conn]struct{}{}, penalty: map[string]time.Time{}, kick: map[string]chan struct{}{}}
	for _, c := range chs {
		f.state[c.ID()] = &ChannelState{ID: c.ID(), Title: c.Title(), Err: "ещё не проверен"}
		f.kick[c.ID()] = make(chan struct{}, 1)
	}
	return f
}

// x224 — запрос подключения RDP (X.224 Connection Request); сервер RDP отвечает Connection Confirm (0x03 0x00 ...).
var x224 = []byte{0x03, 0x00, 0x00, 0x13, 0x0e, 0xe0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00, 0x00}

func probe(c Channel) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	t := time.Now()
	type dialed struct {
		conn net.Conn
		err  error
	}
	ch := make(chan dialed, 1)
	go func() { cn, e := c.Dial(ctx); ch <- dialed{cn, e} }()
	var conn net.Conn
	select {
	case r := <-ch:
		if r.err != nil {
			return 0, r.err
		}
		conn = r.conn
	case <-ctx.Done():
		// закрывать только при успехе: AWG (gvisor) при ошибке отдаёт типизированный nil (*gonet.TCPConn)(nil),
		// он != nil как net.Conn, и Close на нём роняло всю программу (мак 16.11 после сна)
		go func() {
			if r := <-ch; r.err == nil && r.conn != nil {
				r.conn.Close()
			}
		}()
		return 0, errors.New("канал не ответил за 25 с")
	}
	defer conn.Close()
	// соединение xray не соблюдает SetDeadline: без принудительного закрытия чтение висело вечно
	// и останавливало перепроверку всех каналов (probeAll ждёт каждый)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	kill := time.AfterFunc(10*time.Second, func() { conn.Close() })
	defer kill.Stop()
	if _, err := conn.Write(x224); err != nil {
		return 0, err
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		if strings.Contains(err.Error(), "closed pipe") || err == io.EOF { // xray: связь с сервером не установилась
			return 0, errors.New("нет ответа от сервера по TCP — путь до сервера режется")
		}
		return 0, errors.New("RDP не ответил: " + err.Error())
	}
	if !bytes.Equal(buf[:2], []byte{0x03, 0x00}) {
		return 0, errors.New("ответ не похож на RDP")
	}
	return time.Since(t), nil
}

// probeOne — проверить канал и записать результат; true — канал живой.
func (f *Forwarder) probeOne(c Channel) bool {
	d, err := probe(c)
	f.mu.Lock()
	s := f.state[c.ID()]
	s.Checked = time.Now().Format("15:04:05")
	if err != nil {
		if s.OK {
			log.Printf("канал %s упал: %v", c.ID(), err)
		}
		s.OK, s.Ms, s.Err = false, 0, err.Error()
	} else {
		if !s.OK {
			log.Printf("канал %s работает: %.0f мс", c.ID(), d.Seconds()*1000)
		}
		s.OK, s.Ms, s.Err = true, float64(d.Milliseconds()), ""
	}
	f.mu.Unlock()
	if err != nil {
		f.stats.latency(c.ID(), 0)
		c.Reset()
		return false
	}
	f.stats.latency(c.ID(), float64(d.Milliseconds()))
	return true
}

// probeAll — проверить все каналы сразу и дождаться результата (кнопка «Проверить сейчас»).
func (f *Forwarder) probeAll() {
	var wg sync.WaitGroup
	for _, c := range f.channels {
		wg.Add(1)
		go func(c Channel) { defer wg.Done(); f.probeOne(c) }(c)
	}
	wg.Wait()
}

// kickAll — попросить циклы проверки всех каналов проверить сейчас, не дожидаясь своего срока.
func (f *Forwarder) kickAll() {
	for _, k := range f.kick {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

func (f *Forwarder) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// wallNow — настенные часы (Round(0) убирает монотонные: на маке во сне они стоят, и сна было бы не видно).
var wallNow = func() time.Time { return time.Now().Round(0) }

// wakeTick — как часто сторож сна сверяет часы.
var wakeTick = 5 * time.Second

// probeRetry — когда перепроверять неответивший канал: быстро вначале, потом реже (живой — раз в 30 с).
var probeRetry = []time.Duration{3 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second}

// probeChannel — свой цикл у каждого канала: медленный канал не задерживает проверку остальных.
func (f *Forwarder) probeChannel(c Channel) {
	fails := 0
	for !f.isStopped() {
		delay := 30 * time.Second
		if f.probeOne(c) {
			fails = 0
		} else {
			delay = probeRetry[min(fails, len(probeRetry)-1)]
			fails++
		}
		select {
		case <-f.kick[c.ID()]:
			fails = 0
		case <-time.After(delay):
		}
	}
}

// probeLoop — циклы проверки каналов и сторож сна: после сна компьютера (часы прыгнули вперёд) прежние
// «работает» недействительны — туннели мертвы, пока не переподключатся. Сбросить и проверить сразу,
// иначе первое подключение после пробуждения шло бы в мёртвый канал и ждало его 15 с.
func (f *Forwarder) probeLoop() {
	for _, c := range f.channels {
		go f.probeChannel(c)
	}
	last := wallNow()
	for !f.isStopped() {
		time.Sleep(wakeTick)
		now := wallNow()
		if now.Sub(last) > 30*time.Second {
			log.Printf("компьютер проснулся (пауза %s) — перепроверяю каналы", now.Sub(last).Round(time.Second))
			f.mu.Lock()
			for _, s := range f.state {
				s.OK, s.Ms, s.Err = false, 0, "проверяю после сна…"
			}
			f.mu.Unlock()
			for _, c := range f.channels {
				c.Reset()
			}
			f.kickAll()
		}
		last = now
	}
}

// stop — смена профиля: закрыть вход, соединения и все каналы (туннель AWG старого профиля не должен жить в фоне).
func (f *Forwarder) stop() {
	f.disconnect()
	f.mu.Lock()
	f.stopped = true
	if f.ln != nil {
		f.ln.Close()
	}
	chs := f.channels
	f.mu.Unlock()
	for _, c := range chs {
		c.Close()
	}
}

// order — каналы в порядке выбора: в ручном режиме выбранный первым; дальше живые по скорости, потом остальные.
func (f *Forwarder) order() []Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]Channel(nil), f.channels...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := f.state[out[i].ID()], f.state[out[j].ID()]
		if f.mode != "auto" && (a.ID == f.mode) != (b.ID == f.mode) {
			return a.ID == f.mode
		}
		if a.OK != b.OK {
			return a.OK
		}
		if pa, pb := f.penalized(a.ID), f.penalized(b.ID); pa != pb { // недавно оборвавшийся — после остальных живых
			return pb
		}
		// среди живых — по надёжности для долгого соединения, а не по задержке: Reality и SSH идут по TCP
		// напрямую, и из России такой путь рвут посреди сеанса (15.11: дома RDP через Reality оборвался через 15 мин)
		if ra, rb := channelRank[a.ID], channelRank[b.ID]; ra != rb {
			return ra < rb
		}
		return a.OK && a.Ms < b.Ms
	})
	return out
}

// channelRank — порядок выбора живых каналов: AWG (UDP), CDN (через Cloudflare), Reality, SSH.
var channelRank = map[string]int{"awg": 0, "cdn": 1, "xray": 2, "ssh": 3}

func (f *Forwarder) listen() error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(f.port)))
	if err != nil {
		return err
	}
	f.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return nil
}

// usable — каналы для нового соединения: только прошедшие проверку (Dial у xray «успешен» и без связи
// с сервером); если проверенных нет — пробуем все по порядку.
func (f *Forwarder) usable() []Channel {
	for wait := 0; ; wait++ {
		all := f.order()
		f.mu.Lock()
		var ok []Channel
		for _, c := range all {
			if f.state[c.ID()].OK {
				ok = append(ok, c)
			}
		}
		f.mu.Unlock()
		if len(ok) > 0 {
			return ok
		}
		if wait >= 30 { // за 30 с ни один канал не прошёл проверку — пробуем все подряд
			return all
		}
		time.Sleep(time.Second)
	}
}

func (f *Forwarder) handle(local net.Conn) {
	defer local.Close()
	f.mu.Lock()
	f.live[local] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.live, local)
		f.mu.Unlock()
	}()
	for _, ch := range f.usable() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		remote, err := ch.Dial(ctx)
		cancel()
		if err != nil {
			log.Printf("RDP через %s не вышло: %v", ch.ID(), err)
			f.mu.Lock()
			f.state[ch.ID()].OK, f.state[ch.ID()].Err = false, err.Error()
			f.mu.Unlock()
			ch.Reset()
			continue
		}
		log.Printf("RDP-соединение через %s", ch.ID())
		f.mu.Lock()
		f.state[ch.ID()].Active++
		f.mu.Unlock()
		start := time.Now()
		serverSide := pipe(local, countConn{Conn: remote, c: f.stats.ch(ch.ID())})
		f.mu.Lock()
		f.state[ch.ID()].Active--
		f.mu.Unlock()
		if serverSide { // связь пропала со стороны сервера, а окно RDP было открыто — это обрыв, не «Отключиться»
			f.markDrop(ch, time.Since(start))
			return
		}
		log.Printf("RDP-соединение через %s закрыто", ch.ID())
		return
	}
	log.Printf("ни один канал не дошёл до сервера")
}

// pipe — перекладывать данные в обе стороны, пока одна не закроется. true — первой закрылась сторона b
// (для RDP: сервер/канал), то есть соединение оборвалось не по воле окна RDP.
func pipe(a, b net.Conn) bool {
	done := make(chan bool, 2) // true — закончилось чтение из b
	cp := func(dst, src net.Conn, fromB bool) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		}
		done <- fromB
	}
	go cp(a, b, true)
	go cp(b, a, false)
	first := <-done
	a.Close()
	b.Close()
	<-done
	return first
}

// disconnect закрывает все открытые RDP-соединения (окно mstsc закрывается отдельно).
func (f *Forwarder) disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.live {
		c.Close()
	}
}

func (f *Forwarder) activeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.state {
		n += s.Active
	}
	return n
}

func (f *Forwarder) snapshot() []ChannelState {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ChannelState
	for _, c := range f.channels {
		s := *f.state[c.ID()]
		s.Penalty = f.penalized(c.ID())
		out = append(out, s)
	}
	return out
}

// tofuHostKey — ключ сервера SSH запоминается при первом подключении (файл hostkey-<адрес>); если потом он
// изменится — подключение отвергается (защита от подмены).
func tofuHostKey(server string) ssh.HostKeyCallback {
	path := filepath.Join(appDir(), "hostkey-"+server)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := key.Marshal()
		old, err := os.ReadFile(path)
		if err != nil {
			return os.WriteFile(path, got, 0o600)
		}
		if !bytes.Equal(old, got) {
			return errors.New("ключ сервера SSH изменился — подключение остановлено (если сервер переустановлен, удалите " + path + ")")
		}
		return nil
	}
}
