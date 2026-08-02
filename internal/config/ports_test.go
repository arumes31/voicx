package config

import "testing"

func TestDefaultProductPortsStayInCustomRange(t *testing.T) {
	ports := map[string]int{
		"control": DefaultControlPort, "udp": DefaultUDPPort,
		"query": DefaultQueryPort, "file": DefaultFilePort,
		"health": DefaultHealthPort, "grpc": DefaultGRPCPort,
		"query ssh": DefaultQuerySSHPort, "turn": DefaultTURNPort,
		"turn tls": DefaultTURNTLSPort,
	}
	seen := map[int]string{}
	for name, port := range ports {
		if port < 12333 || port > 12366 {
			t.Errorf("%s port %d is outside 12333-12366", name, port)
		}
		if other, duplicate := seen[port]; duplicate {
			t.Errorf("%s and %s both use %d", name, other, port)
		}
		seen[port] = name
	}
	if DefaultTURNRelayMin < 12333 || DefaultTURNRelayMax > 12366 || DefaultTURNRelayMin > DefaultTURNRelayMax {
		t.Errorf("TURN relay range %d-%d is invalid", DefaultTURNRelayMin, DefaultTURNRelayMax)
	}
}
