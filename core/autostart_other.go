//go:build !windows && !darwin

package core

import "errors"

func autostartEnabled() bool  { return false }
func setAutostart(bool) error { return errors.New("на этой платформе автозапуском управляет система") }

func exchangeDir() string { return "" }
func openFolder(string)   {}
