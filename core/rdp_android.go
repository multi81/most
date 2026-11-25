//go:build !windows && !darwin

package core

import (
	"errors"
	"net/url"
	"strconv"
)

// launchRDP — открыть Microsoft «Windows App» ссылкой rdp:// на локальный вход «Моста».
func launchRDP(p *Profile, port int) (rdpProc, error) {
	if openURL == nil {
		return nil, errors.New("не задан способ открыть RDP-клиент")
	}
	u := "rdp://full%20address=s:127.0.0.1:" + strconv.Itoa(port) + "&username=s:" + url.QueryEscape(p.RDPUser) +
		"&audiomode=i:2&prompt%20for%20credentials%20on%20client=i:0"
	if err := openURL(u); err != nil {
		return nil, errors.New("не открылся RDP-клиент — установите Microsoft «Windows App» из Google Play")
	}
	return nil, nil
}

