package core

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRDP — канал, на другом конце которого «сервер RDP» отвечает на X.224.
type fakeRDP struct {
	id     string
	probes atomic.Int32
	resets atomic.Int32
}

func (c *fakeRDP) ID() string    { return c.id }
func (c *fakeRDP) Title() string { return c.id }
func (c *fakeRDP) Reset()        { c.resets.Add(1) }
func (c *fakeRDP) Close()        {}
func (c *fakeRDP) Dial(ctx context.Context) (net.Conn, error) {
	c.probes.Add(1)
	a, b := net.Pipe()
	go func() {
		buf := make([]byte, len(x224))
		if _, err := io.ReadFull(b, buf); err == nil {
			_, _ = b.Write([]byte{0x03, 0x00, 0x00, 0x0b})
		}
		b.Close()
	}()
	return a, nil
}

// После «сна» (часы прыгнули) прежние состояния сбрасываются, каналы перепроверяются сразу.
func TestProbeLoopWake(t *testing.T) {
	var mu sync.Mutex
	shift := time.Duration(0)
	wallNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Round(0).Add(shift) }
	wakeTick = 20 * time.Millisecond

	ch := &fakeRDP{id: "awg"}
	f := newForwarder([]Channel{ch}, 0)
	done := make(chan struct{})
	go func() { f.probeLoop(); close(done) }()
	defer func() { // остановить цикл и дождаться его, потом вернуть часы
		f.mu.Lock()
		f.stopped = true
		f.mu.Unlock()
		<-done
		wallNow = func() time.Time { return time.Now().Round(0) }
		wakeTick = 5 * time.Second
	}()

	waitFor(t, "первая проверка", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.state["awg"].OK })
	before := ch.probes.Load()
	mu.Lock()
	shift = 10 * time.Minute // компьютер «проспал» 10 минут
	mu.Unlock()
	waitFor(t, "сброс после сна", func() bool { return ch.resets.Load() >= 1 })
	waitFor(t, "перепроверка сразу", func() bool { return ch.probes.Load() > before })
	waitFor(t, "снова живой", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.state["awg"].OK })
}

// deafConn — соединение xray без связи: не отвечает и не соблюдает SetDeadline.
type deafConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *deafConn) Read([]byte) (int, error)        { <-c.closed; return 0, io.ErrClosedPipe }
func (c *deafConn) Write(b []byte) (int, error)     { return len(b), nil }
func (c *deafConn) SetDeadline(time.Time) error     { return nil }
func (c *deafConn) Close() error                    { c.once.Do(func() { close(c.closed) }); return nil }

type deafChannel struct{ fakeRDP }

func (c *deafChannel) Dial(context.Context) (net.Conn, error) {
	return &deafConn{closed: make(chan struct{})}, nil
}

// Проверка канала, который молчит и не соблюдает сроки, не висит вечно (0.9.0: стояла вся перепроверка).
func TestProbeDeafChannelTimesOut(t *testing.T) {
	start := time.Now()
	_, err := probe(&deafChannel{fakeRDP{id: "xray"}})
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("проверка шла %s", d)
	}
}

// cleanConn отдаёт настоящий nil при ошибке (типизированный nil ронял программу в 0.9.1).
func TestCleanConnNil(t *testing.T) {
	c, err := cleanConn(nil, errors.New("нет связи"))
	if c != nil || err == nil {
		t.Fatalf("cleanConn: %v %v", c, err)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("не дождалась: %s", what)
}

// mergeXray: пустое с сервера не затирает, изменение замечается, UUID не трогается.
func TestMergeXray(t *testing.T) {
	cur := XrayParams{Port: 443, UUID: "u", PublicKey: "k", ShortID: "s", SNI: "www.samsung.com", WSHost: "cla", WSPort: 8443, WSPath: "/p"}
	if _, ch := mergeXray(cur, XrayParams{}); ch {
		t.Fatal("пустой ответ не должен ничего менять")
	}
	if _, ch := mergeXray(cur, cur); ch {
		t.Fatal("те же параметры — не изменение")
	}
	n, ch := mergeXray(cur, XrayParams{SNI: "site.example.com", UUID: "чужой"})
	if !ch || n.SNI != "site.example.com" || n.UUID != "u" || n.PublicKey != "k" {
		t.Fatalf("слияние: %+v %v", n, ch)
	}
}
