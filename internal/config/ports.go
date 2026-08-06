package config

// Product-owned network ports stay inside one firewall-friendly range. The
// bundled TURN relay pool occupies 12342-12366/udp; TCP services may reuse
// numbers in that subrange without colliding with it.
const (
	DefaultControlPort  = 12333
	DefaultUDPPort      = 12334
	DefaultQueryPort    = 12335
	DefaultFilePort     = 12336
	DefaultHealthPort   = 12337
	DefaultGRPCPort     = 12338
	DefaultQuerySSHPort = 12339
	DefaultTURNPort     = 12340
	DefaultTURNTLSPort  = 12341
	DefaultTURNRelayMin = 12342
	DefaultTURNRelayMax = 12366
)

const (
	DefaultTCPAddr      = ":12333"
	DefaultUDPAddr      = ":12334"
	DefaultQueryAddr    = "127.0.0.1:12335"
	DefaultFileAddr     = ":12336"
	DefaultHealthAddr   = ":12337"
	DefaultGRPCAddr     = "127.0.0.1:12338"
	DefaultQuerySSHAddr = ":12339"
)
