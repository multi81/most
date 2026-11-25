//go:build !windows

package core

// Android, macOS, Linux: каталог данных задаёт оболочка (SetDataDir), профили шифруются AES-256-GCM ключом из
// файла в том же закрытом каталоге (на Android он доступен только самому приложению; на маке позже — Связка ключей).
// RDP открывает внешний клиент по ссылке rdp:// (Microsoft «Windows App») через обработчик оболочки (SetOpenURL).

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type rdpProc interface {
	Wait() error
	Kill() error
}

var (
	dataDirMu sync.Mutex
	dataDir   string
	openURL   func(string) error // открыть ссылку системой (Android: Intent ACTION_VIEW)
)

// SetDataDir — закрытый каталог приложения (Android: Context.getFilesDir()).
func SetDataDir(d string) { dataDirMu.Lock(); dataDir = d; dataDirMu.Unlock() }

// SetOpenURL — как открыть ссылку (rdp://, https://) средствами системы.
func SetOpenURL(f func(string) error) { openURL = f }

func appDir() string {
	dataDirMu.Lock()
	d := dataDir
	dataDirMu.Unlock()
	if d == "" {
		h, _ := os.UserConfigDir()
		d = filepath.Join(h, "Most")
	}
	_ = os.MkdirAll(d, 0o700)
	return d
}

func binDir() string { return filepath.Join(appDir(), "bin") }

func secretKey() ([]byte, error) {
	p := filepath.Join(appDir(), "secret.key")
	if k, err := os.ReadFile(p); err == nil && len(k) == 32 {
		return k, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, os.WriteFile(p, k, 0o600)
}

func sealSecret(b []byte) ([]byte, error) {
	k, err := secretKey()
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(k)
	g, _ := cipher.NewGCM(blk)
	n := make([]byte, g.NonceSize())
	_, _ = rand.Read(n)
	return g.Seal(n, n, b, nil), nil
}

func openSecret(b []byte) ([]byte, error) {
	k, err := secretKey()
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(k)
	g, _ := cipher.NewGCM(blk)
	if len(b) < g.NonceSize() {
		return nil, errors.New("профиль повреждён")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
}

func platformQuit()           {}
func updatesSupported() bool  { return false }
func startExe(string) error   { return errors.New("обновление на этой платформе — через магазин или APK") }
func chromePath() string      { return "" }
func claudeAvailable() bool   { return false }
func launchChrome() error     { return errors.New("на этой платформе недоступно") }
func launchClaude() error     { return errors.New("на этой платформе недоступно") }

// rdpHint — что показать после запуска: пароль нужно один раз сохранить в «Windows App».
func rdpHint(p *Profile) string { return p.RDPPassword }
