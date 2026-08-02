// queryload drives the ServerQuery TCP interface at a fixed aggregate rate.
// It uses persistent authenticated connections and reports achieved rate,
// failures and latency percentiles; the default profile is backlog item 934.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type options struct {
	addr, user, password, command string
	rate, connections             int
	duration                      time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:10012", "ServerQuery address")
	flag.StringVar(&o.user, "user", os.Getenv("VOICX_QUERY_USER"), "admin unique ID (or VOICX_QUERY_USER)")
	flag.StringVar(&o.password, "password", os.Getenv("VOICX_QUERY_PASSWORD"), "admin password (or VOICX_QUERY_PASSWORD)")
	flag.StringVar(&o.command, "command", "clientlist", "query command to execute")
	flag.IntVar(&o.rate, "rate", 5000, "target aggregate requests per second")
	flag.IntVar(&o.connections, "connections", 64, "persistent query connections")
	flag.DurationVar(&o.duration, "duration", 15*time.Second, "load duration")
	flag.Parse()
	if o.user == "" || o.password == "" || o.rate < 1 || o.connections < 1 || o.duration <= 0 {
		fmt.Fprintln(os.Stderr, "queryload: user/password, positive rate, connections and duration are required")
		os.Exit(2)
	}
	if err := run(context.Background(), o); err != nil {
		fmt.Fprintln(os.Stderr, "queryload:", err)
		os.Exit(1)
	}
}

type result struct {
	ok, failed atomic.Int64
	mu         sync.Mutex
	latency    []time.Duration
}

func run(parent context.Context, o options) error {
	ctx, cancel := context.WithTimeout(parent, o.duration)
	defer cancel()
	jobs := make(chan struct{}, o.connections*2)
	res := &result{latency: make([]time.Duration, 0, o.rate*int(o.duration/time.Second))}
	var wg sync.WaitGroup
	for i := 0; i < o.connections; i++ {
		conn, reader, err := dialAndLogin(o)
		if err != nil {
			cancel()
			return fmt.Errorf("connection %d: %w", i, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			for range jobs {
				start := time.Now()
				if _, err := fmt.Fprintln(conn, o.command); err != nil || readResponse(reader) != nil {
					res.failed.Add(1)
					continue
				}
				res.ok.Add(1)
				res.mu.Lock()
				res.latency = append(res.latency, time.Since(start))
				res.mu.Unlock()
			}
		}()
	}

	interval := time.Second / time.Duration(o.rate)
	ticker := time.NewTicker(interval)
	for {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			printReport(o, res)
			return nil
		case <-ticker.C:
			select {
			case jobs <- struct{}{}:
			default:
				res.failed.Add(1) // saturation: count a request that could not be scheduled
			}
		}
	}
}

func dialAndLogin(o options) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", o.addr, 5*time.Second)
	if err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReaderSize(conn, 64*1024)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := reader.ReadString('\n'); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if _, err := reader.ReadString('\n'); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if _, err := fmt.Fprintf(conn, "login %s %s\n", escape(o.user), escape(o.password)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := readResponse(reader); err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, nil
}

func readResponse(reader *bufio.Reader) error {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, "error id=") {
			if !strings.HasPrefix(line, "error id=0 ") {
				return fmt.Errorf("query response: %s", strings.TrimSpace(line))
			}
			return nil
		}
	}
}

func escape(value string) string {
	r := strings.NewReplacer("\\", `\\`, " ", `\s`, "|", `\p`, "/", `\/`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return r.Replace(value)
}

func printReport(o options, r *result) {
	r.mu.Lock()
	latency := append([]time.Duration(nil), r.latency...)
	r.mu.Unlock()
	sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
	percentile := func(p float64) time.Duration {
		if len(latency) == 0 {
			return 0
		}
		idx := int(float64(len(latency)-1) * p)
		return latency[idx]
	}
	ok := r.ok.Load()
	fmt.Printf("queryload target=%d/s achieved=%.0f/s ok=%d failed=%d p50=%s p95=%s p99=%s\n",
		o.rate, float64(ok)/o.duration.Seconds(), ok, r.failed.Load(), percentile(.50), percentile(.95), percentile(.99))
}
