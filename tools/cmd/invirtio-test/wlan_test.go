package main

import (
	"fmt"
	"testing"
)

const wlanPhyInfo = `Wiphy phy1
	Supported Ciphers:
		* WEP40 (00-0f-ac:1)
		* CCMP-128 (00-0f-ac:4)
	Band 1:
		Capabilities: 0x1ff
		Frequencies:
			* 2412.0 MHz [1] (20.0 dBm)
			* 2417 MHz [2] (20.0 dBm)
		EHT Iftypes: managed
	Band 2:
		Capabilities: 0x2ff
		VHT Capabilities (0x339071b2):
		HE Iftypes: managed, AP
		Frequencies:
			* 5180.0 MHz [36] (20.0 dBm)
	Supported interface modes:
		 * managed
`

func TestParsePhy(t *testing.T) {
	bands, ciphers := parsePhy(wlanPhyInfo)
	b1, b2 := bands[1], bands[2]
	if b1 == nil || b2 == nil || len(bands) != 2 {
		t.Fatalf("bands = %v", bands)
	}
	if fmt.Sprint(b1.freqs) != "[2412 2417]" || b1.ht != "0x1ff" || b1.vht != "" || b1.he {
		t.Errorf("band 1 = %+v", *b1)
	}
	if fmt.Sprint(b2.freqs) != "[5180]" || b2.ht != "0x2ff" || b2.vht != "0x339071b2" || !b2.he {
		t.Errorf("band 2 = %+v", *b2)
	}
	if fmt.Sprint(ciphers) != "[CCMP-128 (00-0f-ac:4) WEP40 (00-0f-ac:1)]" {
		t.Errorf("ciphers = %q", ciphers)
	}
}

func TestParseScan(t *testing.T) {
	found := parseScan(`BSS 02:00:00:00:01:00(on wlan5)
	freq: 2412
	SSID: hwsim-2g-open
BSS 02:00:00:00:02:00(on wlan5)
	freq: 5180.0
	SSID: hwsim-5g-open
`)
	if found["hwsim-2g-open"] != 2412 || found["hwsim-5g-open"] != 5180 || len(found) != 2 {
		t.Errorf("found = %v", found)
	}
	if bandOf(2412) != "2g" || bandOf(5180) != "5g" || bandOf(5955) != "6g" {
		t.Error("bandOf")
	}
}
