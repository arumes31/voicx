package webrtc

import (
	"math"
	"testing"
	"time"
)

func TestUsableICEInterface(t *testing.T) {
	for _, name := range []string{"Ethernet", "Wi-Fi", "lo", "wg0", "tun0"} {
		if !usableICEInterface(name) {
			t.Errorf("usableICEInterface(%q) = false", name)
		}
	}
	for _, name := range []string{"docker0", "veth42", "virbr0", "vboxnet0", "vmnet8", "br-deadbeef"} {
		if usableICEInterface(name) {
			t.Errorf("usableICEInterface(%q) = true", name)
		}
	}
}

func TestEngineDTLSCertificateIsStableForRun(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.DTLSFingerprint() == "" {
		t.Fatal("empty DTLS fingerprint")
	}
	a, err := e.NewPeerConnection("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.NewPeerConnection("b")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.pc.GetConfiguration().Certificates) != 1 || len(b.pc.GetConfiguration().Certificates) != 1 {
		t.Fatal("peer connections did not receive the engine certificate")
	}
}

func TestKeyframeBurstSuppression(t *testing.T) {
	r := NewRouter(nil)
	now := time.Now()
	if !r.allowKeyframeRequest(7, now) {
		t.Fatal("first request rejected")
	}
	if r.allowKeyframeRequest(7, now.Add(500*time.Millisecond)) {
		t.Fatal("duplicate request inside window accepted")
	}
	if !r.allowKeyframeRequest(7, now.Add(time.Second)) {
		t.Fatal("request after window rejected")
	}
}

func TestSoftClipMix(t *testing.T) {
	dst := make([]float32, 3)
	SoftClipMix(dst, []float32{0.5, 1, -1}, []float32{0.5, 1, -1})
	for i, v := range dst {
		if v < -1 || v > 1 || math.IsNaN(float64(v)) {
			t.Fatalf("sample %d = %v", i, v)
		}
	}
	if !(dst[1] < 1 && dst[1] > dst[0]) {
		t.Fatalf("limiter did not soft-clip monotonically: %v", dst)
	}
}
