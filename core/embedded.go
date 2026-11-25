package core

// Запуск ядра внутри чужой оболочки (Android-приложение, позже — мак): локальная страница на случайном
// свободном порту 127.0.0.1, тот же API и то же окно, что и на Windows. Оболочка показывает адрес в WebView.

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var embedded *App

// StartEmbedded — поднять ядро; возвращает адрес окна (с ключом доступа). Повторный вызов отдаёт тот же адрес.
func StartEmbedded() (string, error) {
	if embedded != nil {
		return embedded.url, nil
	}
	if f, err := os.OpenFile(filepath.Join(appDir(), "most.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(f)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	app := &App{token: hex.EncodeToString(b), started: time.Now()}
	app.url = "http://" + ln.Addr().String() + "/?t=" + app.token
	log.Printf("Мост %s запущен (встроенный)", version)
	if p, err := loadProfile(); err == nil {
		app.use(p)
	}
	mux := http.NewServeMux()
	app.routes(mux)
	go func() { _ = http.Serve(ln, mux) }()
	go app.paramsLoop()
	embedded = app
	return app.url, nil
}

// Version — версия ядра (для оболочки).
func Version() string { return version }
