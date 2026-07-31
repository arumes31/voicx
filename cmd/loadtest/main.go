// loadtest is a headless voicx client simulator for load testing. Each
// simulated client connects over the TCP control channel, authenticates
// (password path with a shared test account), joins a channel, sends global
// chat and pings, and optionally sends UDP pings to exercise the UDP path
// and its rate limiter.
//
// Usage:
//
//	loadtest -addr 127.0.0.1:10011 -clients 50 -duration 30s -ramp 5s \
//	    -unique-id <uid> -password <pw> [-channel 1] [-udp -udp-addr 127.0.0.1:9987]
//
// Authentication uses a single shared account for all simulated clients
// (voicx allows multiple connections per unique ID). Create a test user
// first (there is no protocol-level registration; use psql or an admin
// token flow), e.g. via a one-off Go snippet calling auth.RegisterUser.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"voicx/internal/netproto"
)

// options holds the load-test parameters.
type options struct {
	addr      string
	udpAddr   string
	clients   int
	duration  time.Duration
	ramp      time.Duration
	uniqueID  string
	password  string
	channel   int64
	udp       bool
	anonymous bool
}

// stats accumulates load-test results.
type stats struct {
	connectsOK   atomic.Int64
	connectsFail atomic.Int64
	authOK       atomic.Int64
	authFail     atomic.Int64
	chatSent     atomic.Int64
	chatRecv     atomic.Int64
	pongs        atomic.Int64

	// authLatencyBuckets: <10ms, <50ms, <100ms, <500ms, <1s, >=1s.
	authLatency [6]atomic.Int64
}

// bucketLatency records an auth round-trip latency.
func (s *stats) bucketLatency(d time.Duration) {
	var i int
	switch {
	case d < 10*time.Millisecond:
		i = 0
	case d < 50*time.Millisecond:
		i = 1
	case d < 100*time.Millisecond:
		i = 2
	case d < 500*time.Millisecond:
		i = 3
	case d < time.Second:
		i = 4
	default:
		i = 5
	}
	s.authLatency[i].Add(1)
}

// print writes the final report.
func (s *stats) print(opts options) {
	fmt.Println("--- loadtest report ---")
	fmt.Printf("clients=%d duration=%s ramp=%s\n", opts.clients, opts.duration, opts.ramp)
	fmt.Printf("connects: ok=%d fail=%d\n", s.connectsOK.Load(), s.connectsFail.Load())
	fmt.Printf("auth:     ok=%d fail=%d\n", s.authOK.Load(), s.authFail.Load())
	fmt.Printf("chat:     sent=%d received=%d\n", s.chatSent.Load(), s.chatRecv.Load())
	fmt.Printf("pongs:    %d\n", s.pongs.Load())
	fmt.Printf("auth latency (ms): <10=%d <50=%d <100=%d <500=%d <1000=%d >=1000=%d\n",
		s.authLatency[0].Load(), s.authLatency[1].Load(), s.authLatency[2].Load(),
		s.authLatency[3].Load(), s.authLatency[4].Load(), s.authLatency[5].Load())
}

func main() {
	var opts options
	flag.StringVar(&opts.addr, "addr", "127.0.0.1:10011", "control channel address")
	flag.StringVar(&opts.udpAddr, "udp-addr", "127.0.0.1:9987", "UDP media address (with -udp)")
	flag.IntVar(&opts.clients, "clients", 10, "number of simulated clients")
	flag.DurationVar(&opts.duration, "duration", 30*time.Second, "load duration")
	flag.DurationVar(&opts.ramp, "ramp", 5*time.Second, "ramp-up period over which clients start")
	flag.StringVar(&opts.uniqueID, "unique-id", "", "account unique ID (shared by all clients)")
	flag.StringVar(&opts.password, "password", "", "account password")
	flag.Int64Var(&opts.channel, "channel", 1, "channel ID to join (0 = don't join)")
	flag.BoolVar(&opts.udp, "udp", false, "also send UDP pings to exercise the UDP path")
	flag.BoolVar(&opts.anonymous, "anonymous", false, "connect as anonymous guests (loadtest-N nicknames; -unique-id/-password not needed)")
	flag.Parse()

	if !opts.anonymous && (opts.uniqueID == "" || opts.password == "") {
		fmt.Fprintln(os.Stderr, "loadtest: -unique-id and -password are required (or use -anonymous)")
		os.Exit(2)
	}
	if opts.clients < 1 {
		fmt.Fprintln(os.Stderr, "loadtest: -clients must be >= 1")
		os.Exit(2)
	}

	var st stats
	if err := run(context.Background(), opts, &st); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(1)
	}
	st.print(opts)
}

// run executes the load test: it starts opts.clients simulated clients,
// staggered over the ramp period, waits for the duration, and returns.
func run(ctx context.Context, opts options, st *stats) error {
	ctx, cancel := context.WithTimeout(ctx, opts.duration)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < opts.clients; i++ {
		// Stagger starts over the ramp period.
		if opts.ramp > 0 && i > 0 {
			delay := time.Duration(i) * opts.ramp / time.Duration(opts.clients)
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			case <-time.After(delay):
			}
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			simulateClient(ctx, opts, st, i)
		}(i)
	}
	wg.Wait()
	return nil
}

// simulateClient is one simulated client connection lifecycle.
func simulateClient(ctx context.Context, opts options, st *stats, index int) {
	conn, err := net.DialTimeout("tcp", opts.addr, 5*time.Second)
	if err != nil {
		st.connectsFail.Add(1)
		return
	}
	defer conn.Close()
	st.connectsOK.Add(1)

	// Authenticate: anonymous guest (loadtest-N) or the shared account.
	authMsg := netproto.Authenticate{Username: opts.uniqueID, Password: opts.password}
	if opts.anonymous {
		authMsg = netproto.Authenticate{Anonymous: true, Nickname: fmt.Sprintf("loadtest-%d", index)}
	}
	start := time.Now()
	if err := writeMsg(conn, netproto.MsgAuthenticate, authMsg); err != nil {
		st.authFail.Add(1)
		return
	}
	f, err := readOfType(conn, netproto.MsgAuthResponse, 5*time.Second)
	if err != nil {
		st.authFail.Add(1)
		return
	}
	st.bucketLatency(time.Since(start))
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil || !resp.OK {
		st.authFail.Add(1)
		return
	}
	st.authOK.Add(1)

	// Consume the snapshot.
	if _, err := readOfType(conn, netproto.MsgSnapshot, 5*time.Second); err != nil {
		return
	}

	// Join the channel (best effort).
	if opts.channel != 0 {
		_ = writeMsg(conn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: opts.channel})
	}

	// UDP pings (optional): one packet per second.
	var udpDone chan struct{}
	if opts.udp {
		udpDone = make(chan struct{})
		go udpPinger(opts.udpAddr, udpDone)
		defer close(udpDone)
	}

	// Reader: count incoming pongs and chat events.
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := netproto.ReadFrame(conn)
			if err != nil {
				readErr <- err
				return
			}
			switch netproto.MessageType(f.Type) {
			case netproto.MsgPong:
				st.pongs.Add(1)
			case netproto.MsgEvent:
				st.chatRecv.Add(1)
			}
		}
	}()

	chatTicker := time.NewTicker(2 * time.Second)
	defer chatTicker.Stop()
	pingTicker := time.NewTicker(5 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-readErr:
			return
		case <-chatTicker.C:
			if err := writeMsg(conn, netproto.MsgChatSend, netproto.ChatSend{Text: "loadtest ping"}); err != nil {
				return
			}
			st.chatSent.Add(1)
		case <-pingTicker.C:
			if err := writeMsg(conn, netproto.MsgPing, netproto.Ping{}); err != nil {
				return
			}
		}
	}
}

// udpPinger sends one UDP ping per second to addr until done closes.
func udpPinger(addr string, done chan struct{}) {
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return
	}
	defer conn.Close()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			_, _ = conn.Write([]byte{netproto.UDPMsgPing})
		}
	}
}

// writeMsg encodes and writes one control message.
func writeMsg(conn net.Conn, mt netproto.MessageType, msg any) error {
	f, err := netproto.Encode(mt, msg)
	if err != nil {
		return err
	}
	return netproto.WriteFrame(conn, f)
}

// readOfType reads frames until one of the wanted type arrives or the
// deadline passes.
func readOfType(conn net.Conn, mt netproto.MessageType, timeout time.Duration) (*netproto.Frame, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		if netproto.MessageType(f.Type) == mt {
			return f, nil
		}
	}
}
