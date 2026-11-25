//go:build windows

// Точка входа «Моста» для Windows: всё ядро — в most/core, здесь только запуск.
package main

import "most/core"

func main() { core.RunDesktop() }
