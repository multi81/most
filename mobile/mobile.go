// Package mobile — ядро «Моста» для Android (gomobile bind → most.aar). Оболочка на Kotlin вызывает Start
// и показывает возвращённый адрес во встроенном окне (WebView); ссылки rdp:// открывает через Opener.
package mobile

import "most/core"

// Opener — реализуется на стороне Android: открыть ссылку системой (Intent ACTION_VIEW).
type Opener interface {
	Open(url string) error
}

// Start — запустить ядро; dataDir — закрытый каталог приложения. Возвращает адрес окна.
func Start(dataDir string, op Opener) (string, error) {
	core.SetDataDir(dataDir)
	if op != nil {
		core.SetOpenURL(op.Open)
	}
	return core.StartEmbedded()
}

// Version — версия ядра.
func Version() string { return core.Version() }
