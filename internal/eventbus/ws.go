// ws.go exposes the event bus over WebSocket so bots can subscribe (231).
// The stream is a firehose of who-is-where, so it is authenticated with the
// same admin credentials as ServerQuery; an anonymous subscriber would be a
// presence-tracking leak.
package eventbus

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/websocket"
)

// Authenticator verifies bot credentials. It has the same shape as the
// ServerQuery login check: ok reports valid credentials, admin reports whether
// the account may administer the server.
type Authenticator func(ctx context.Context, uniqueID, password string) (ok, admin bool, err error)

// wsWriteTimeout bounds a single frame write. A consumer whose TCP window is
// closed must not pin the goroutine forever; the bus drop policy handles the
// backlog, this handles the socket.
const wsWriteTimeout = 10 * time.Second

// wsEvent is the wire form of an event. Data is passed through verbatim, so a
// bot sees exactly the payload connected clients see.
type wsEvent struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Time string          `json:"time"`
	Data json.RawMessage `json:"data"`
}

// Handler serves the WebSocket event stream at whatever path it is mounted on.
// Subscribers authenticate with HTTP Basic auth and may narrow the stream with
// a comma-separated ?types= query parameter.
func Handler(bus *Bus, auth Authenticator, logger *zap.Logger) http.Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="voicx events"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		valid, admin, err := auth(r.Context(), user, password)
		if err != nil {
			logger.Warn("event stream auth error", zap.Error(err))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !valid || !admin {
			logger.Warn("event stream login refused",
				zap.String("unique_id", user), zap.String("remote", r.RemoteAddr))
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}

		types := parseTypes(r.URL.Query().Get("types"))
		websocket.Handler(func(conn *websocket.Conn) {
			defer conn.Close()
			serveStream(bus, conn, user, types, logger)
		}).ServeHTTP(w, r)
	})
}

// parseTypes splits the ?types= filter; an empty parameter means all events.
func parseTypes(raw string) []string {
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// serveStream pumps bus events into one WebSocket connection until either side
// goes away.
func serveStream(bus *Bus, conn *websocket.Conn, name string, types []string, logger *zap.Logger) {
	sub := bus.Subscribe("ws:"+name, types, 0)
	if sub == nil {
		return
	}
	defer sub.Unsubscribe()

	// A subscriber that never reads still has to be noticed when it hangs up,
	// so drain (and discard) its side of the socket.
	peerGone := make(chan struct{})
	go func() {
		defer close(peerGone)
		var discard []byte
		for {
			if err := websocket.Message.Receive(conn, &discard); err != nil {
				return
			}
		}
	}()

	logger.Info("event stream subscriber connected",
		zap.String("unique_id", name), zap.Strings("types", types))
	defer func() {
		logger.Info("event stream subscriber disconnected",
			zap.String("unique_id", name), zap.Uint64("dropped", sub.Dropped()))
	}()

	for {
		select {
		case <-peerGone:
			return
		case evt, ok := <-sub.C:
			if !ok {
				// Evicted by the drop policy: closing the socket tells the bot
				// its stream is no longer complete.
				return
			}
			payload, err := json.Marshal(wsEvent{
				Seq:  evt.Seq,
				Type: evt.Type,
				Time: evt.Time.Format(time.RFC3339Nano),
				Data: evt.Data,
			})
			if err != nil {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := websocket.Message.Send(conn, string(payload)); err != nil {
				return
			}
		}
	}
}
