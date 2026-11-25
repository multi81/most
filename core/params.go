package core

// Параметры Xray с сервера (17.11): сервер может сменить маскировку Reality (имя сайта, ключ) или адрес CDN —
// «Мост» сам забирает текущие параметры у most-admin (/api/params, доступен только изнутри канала) и обновляет
// профиль. Пересоздаёт каналы только когда нет открытых соединений, чтобы не оборвать рабочий стол.

import (
	"log"
	"time"
)

func (a *App) paramsLoop() {
	time.Sleep(30 * time.Second)
	for {
		if !a.refreshParams() {
			time.Sleep(5 * time.Minute) // не дошли до сервера или были открытые соединения — попробовать скоро снова
			continue
		}
		time.Sleep(6 * time.Hour)
	}
}

// refreshParams — false: стоит повторить скоро (сервер недоступен или обновление отложено).
func (a *App) refreshParams() bool {
	a.mu.Lock()
	p, f := a.profile, a.fwd
	a.mu.Unlock()
	if p == nil || f == nil || p.Xray == nil {
		return true
	}
	var x XrayParams
	if err := adminCall(f.channels, "GET", "/api/params", "", nil, &x); err != nil {
		return false
	}
	nx, changed := mergeXray(*p.Xray, x)
	if !changed {
		return true
	}
	if f.busy() {
		log.Printf("сервер сменил параметры Xray — обновлю профиль, когда закроется рабочий стол")
		return false
	}
	np := *p
	np.Xray = &nx
	if err := writeProfile(profileID(&np), &np); err != nil {
		log.Printf("профиль не сохранился: %v", err)
		return false
	}
	log.Printf("сервер сменил параметры Xray (маскировка: %s) — профиль обновлён, каналы пересозданы", nx.SNI)
	a.mu.Lock()
	same := a.profile == p
	a.mu.Unlock()
	if same {
		a.switchTo(&np)
	}
	return true
}

// mergeXray — взять с сервера только то, что он прислал (пустое не затирает); UUID не меняется.
func mergeXray(cur, srv XrayParams) (XrayParams, bool) {
	changed := false
	str := func(dst *string, v string) {
		if v != "" && *dst != v {
			*dst, changed = v, true
		}
	}
	num := func(dst *int, v int) {
		if v > 0 && *dst != v {
			*dst, changed = v, true
		}
	}
	str(&cur.SNI, srv.SNI)
	str(&cur.PublicKey, srv.PublicKey)
	str(&cur.ShortID, srv.ShortID)
	num(&cur.Port, srv.Port)
	str(&cur.WSHost, srv.WSHost)
	str(&cur.WSPath, srv.WSPath)
	num(&cur.WSPort, srv.WSPort)
	return cur, changed
}

// busy — есть открытые RDP-соединения.
func (f *Forwarder) busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live) > 0
}
