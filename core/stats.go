package core

// Учёт трафика и история для графиков: раз в секунду — скорость приёма/отдачи (всего и по каналам),
// после каждой проверки — задержка канала. Хранится последние historyLen точек.

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const historyLen = 120

type counters struct{ rx, tx atomic.Int64 } // rx — от сервера к нам, tx — от нас к серверу

type Stats struct {
	mu      sync.Mutex
	byCh    map[string]*counters
	lastRx  map[string]int64
	lastTx  map[string]int64
	RxRate  []float64            // байт/с, всего
	TxRate  []float64            // байт/с, всего
	Latency map[string][]float64 // мс по каналам (0 — канал не ответил)
	Total   [2]int64             // всего за работу программы: rx, tx
}

func newStats() *Stats {
	s := &Stats{byCh: map[string]*counters{}, lastRx: map[string]int64{}, lastTx: map[string]int64{}, Latency: map[string][]float64{}}
	go s.loop()
	return s
}

func (s *Stats) ch(id string) *counters {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byCh[id]
	if c == nil {
		c = &counters{}
		s.byCh[id] = c
	}
	return c
}

func push(a []float64, v float64) []float64 {
	a = append(a, v)
	if len(a) > historyLen {
		a = a[len(a)-historyLen:]
	}
	return a
}

func (s *Stats) loop() {
	for range time.Tick(time.Second) {
		s.mu.Lock()
		var rx, tx int64
		for id, c := range s.byCh {
			r, t := c.rx.Load(), c.tx.Load()
			rx += r - s.lastRx[id]
			tx += t - s.lastTx[id]
			s.lastRx[id], s.lastTx[id] = r, t
		}
		s.RxRate, s.TxRate = push(s.RxRate, float64(rx)), push(s.TxRate, float64(tx))
		s.Total[0] += rx
		s.Total[1] += tx
		s.mu.Unlock()
	}
}

func (s *Stats) latency(id string, ms float64) {
	s.mu.Lock()
	s.Latency[id] = push(s.Latency[id], ms)
	s.mu.Unlock()
}

type statsView struct {
	Rx      []float64            `json:"rx"`
	Tx      []float64            `json:"tx"`
	Latency map[string][]float64 `json:"latency"`
	TotalRx int64                `json:"total_rx"`
	TotalTx int64                `json:"total_tx"`
}

func (s *Stats) view() statsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	lat := map[string][]float64{}
	for k, v := range s.Latency {
		lat[k] = append([]float64(nil), v...)
	}
	return statsView{Rx: append([]float64(nil), s.RxRate...), Tx: append([]float64(nil), s.TxRate...), Latency: lat,
		TotalRx: s.Total[0], TotalTx: s.Total[1]}
}

// countConn считает байты, прошедшие через соединение с сервером.
type countConn struct {
	net.Conn
	c *counters
}

func (c countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.c.rx.Add(int64(n))
	return n, err
}

func (c countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.c.tx.Add(int64(n))
	return n, err
}

func (c countConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
