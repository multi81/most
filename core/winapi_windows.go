//go:build windows

package core

// Шифрование профиля (DPAPI) и запись пароля RDP в диспетчер учётных данных Windows (то же, что cmdkey /generic,
// но пароль не появляется в командной строке процесса).

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32              = windows.NewLazySystemDLL("crypt32.dll")
	procCryptProtect     = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect   = crypt32.NewProc("CryptUnprotectData")
	advapi32             = windows.NewLazySystemDLL("advapi32.dll")
	procCredWriteW       = advapi32.NewProc("CredWriteW")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procLocalFree        = kernel32.NewProc("LocalFree")
	dpapiEntropy         = []byte("most-profile-v1")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

func (b *dataBlob) bytes() []byte {
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

func dpapiEncrypt(data []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procCryptProtect.Call(uintptr(unsafe.Pointer(newBlob(data))), 0,
		uintptr(unsafe.Pointer(newBlob(dpapiEntropy))), 0, 0, 0x1 /*UI_FORBIDDEN*/, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

func dpapiDecrypt(data []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procCryptUnprotect.Call(uintptr(unsafe.Pointer(newBlob(data))), 0,
		uintptr(unsafe.Pointer(newBlob(dpapiEntropy))), 0, 0, 0x1, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("профиль не расшифровать (создан другим пользователем Windows?): " + err.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// saveRDPCredential — пароль для mstsc: TERMSRV/<host>, тип GENERIC (как cmdkey /generic), хранится в профиле пользователя.
func saveRDPCredential(host, user, password string) error {
	target, _ := windows.UTF16PtrFromString("TERMSRV/" + host)
	u, _ := windows.UTF16PtrFromString(user)
	pw, _ := windows.UTF16FromString(password)
	blob := unsafe.Slice((*byte)(unsafe.Pointer(&pw[0])), (len(pw)-1)*2) // без завершающего нуля
	c := credential{Type: 1 /*GENERIC*/, TargetName: target, UserName: u, Persist: 2, /*LOCAL_MACHINE*/
		CredentialBlobSize: uint32(len(blob))}
	if len(blob) > 0 {
		c.CredentialBlob = &blob[0]
	}
	r, _, err := procCredWriteW.Call(uintptr(unsafe.Pointer(&c)), 0)
	if r == 0 {
		return err
	}
	return nil
}
