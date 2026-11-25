//go:build linux && !android

package core

import "errors"

// Linux (без Android) — только для проверок на сервере: открывать окно RDP здесь нечем.
func launchRDP(p *Profile, port int) (rdpProc, error) {
	return nil, errors.New("на Linux окно RDP не открывается")
}
