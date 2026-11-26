package core

import "testing"

// данные установленного сервера для мастера: имя без «(устройство)», порты из профиля, свой домен, CDN
func TestSetupInfo(t *testing.T) {
	p := &Profile{Name: "Германия (pc)", Server: "203.0.113.10", RDPUser: "cloud",
		SSH:  &SSHParams{User: "cloud", Port: 2222},
		AWG:  &AWGParams{Conf: "[Interface]\nPrivateKey = __LOCAL__\n[Peer]\nEndpoint = 203.0.113.10:51820\n"},
		Xray: &XrayParams{SNI: "site.example.com", WSHost: "vpn.example.com"}}
	si := setupInfo(p)
	want := SetupInfo{Host: "203.0.113.10", Port: 2222, Name: "Германия", Desktop: true, AWGPort: 51820,
		Domain: "vpn.example.com", RealityDomain: "site.example.com"}
	if si != want {
		t.Fatalf("получено %+v, ждали %+v", si, want)
	}
	// маскировка по умолчанию — не свой домен; без SSH — порт 22; без рабочего стола
	si = setupInfo(&Profile{Name: "Офис", Server: "198.51.100.5", Xray: &XrayParams{SNI: "www.samsung.com"}})
	if si.RealityDomain != "" || si.Port != 22 || si.Desktop || si.AWGPort != 443 || si.Name != "Офис" {
		t.Fatalf("по умолчанию: %+v", si)
	}
}
