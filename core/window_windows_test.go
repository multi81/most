//go:build windows

package core

import "testing"

// Поиск окна много раз подряд не должен исчерпывать обратные вызовы Windows (раньше на каждый поиск
// создавался новый syscall.NewCallback — после ~2000 программа падала).
func TestFindWindowManyCalls(t *testing.T) {
	for i := 0; i < 3000; i++ {
		findWindow()
	}
}
