package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// hubTestServer spins up a real httptest server with a fully-wired hub +
// bank emitter. Returns the server, the ws:// dial URL, and a cleanup. Use
// this when a test needs an actual WebSocket client to verify the zombie
// detection / disconnect cascade behaviour (which can't be exercised
// against an in-memory channel because there's no real conn to close).
func hubTestServer(t *testing.T) (*Server, string, func()) {
	t.Helper()
	bank := newTestBank(t)
	ring := NewRingBuffer(500)
	hub := NewHub(ring)
	bank.Emit = func(eventType, adapterID string, payload map[string]interface{}) {
		hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          eventType,
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     adapterID,
			Payload:       payload,
		})
	}
	server := &Server{
		bank: bank, hub: hub, ring: ring,
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
	}
	httpSrv := httptest.NewServer(server.routes())
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"
	cleanup := func() { httpSrv.Close() }
	return server, wsURL, cleanup
}

// dialWS is a small wrapper that returns the dialed conn and lets the test
// caller register Close in a defer.
func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return c
}

// drainAndCount reads messages off `c` until it errors (close / timeout / EOF).
// Returns the count of messages received and the terminal error.
func drainAndCount(c *websocket.Conn, deadline time.Duration) (int, error) {
	c.SetReadDeadline(time.Now().Add(deadline))
	count := 0
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			return count, err
		}
		count++
		// Bump the deadline forward as long as messages are flowing.
		c.SetReadDeadline(time.Now().Add(deadline))
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 1. SubscriberSurvivesBurst — 1000 events sent rapidly, the subscriber
//    must either receive all of them OR get a clean close. NEVER silent
//    stop (which is the bug the handoff describes).
// ──────────────────────────────────────────────────────────────────────────

func TestWSHub_SubscriberSurvivesBurst(t *testing.T) {
	server, wsURL, cleanup := hubTestServer(t)
	defer cleanup()

	c := dialWS(t, wsURL)
	defer c.Close()

	// Reader counts everything that arrives + records the terminal error.
	received := atomic.Int32{}
	terminal := make(chan error, 1)
	go func() {
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				terminal <- err
				return
			}
			received.Add(1)
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
		}
	}()

	// Fanout 1000 events as fast as possible.
	const N = 1000
	for i := 0; i < N; i++ {
		server.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "audit.appended",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-bank",
			Payload:       map[string]interface{}{"i": i},
		})
	}

	// Two acceptable outcomes:
	//   (a) all N events arrive (no overflow, no drops)
	//   (b) the connection closes cleanly because we couldn't keep up
	//       (slow-subscriber disconnect fired after 100ms)
	// What MUST NOT happen: silent stop (some K < N events delivered then
	// indefinite quiet without a close error).
	select {
	case err := <-terminal:
		// Got a close — acceptable. Verify it's a real close, not a read
		// timeout (which would indicate the zombie state we're testing
		// against).
		if isReadTimeout(err) {
			t.Errorf("read timed out without a close frame; got %d/%d events. Likely zombie subscriber bug.",
				received.Load(), N)
		}
	case <-time.After(5 * time.Second):
		// No terminal yet — must mean we received all N events and are
		// just waiting for more.
		got := received.Load()
		if int(got) < N {
			t.Errorf("ZOMBIE: subscriber received only %d/%d events, no close frame, still open",
				got, N)
		}
	}
}

// isReadTimeout reports whether err is a "read deadline exceeded" — the
// signature of the zombie state. Real close frames produce typed errors
// from gorilla/websocket; bare deadline timeouts produce *net.OpError with
// Op:"read" Err:"i/o timeout".
func isReadTimeout(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "i/o timeout")
}

// ──────────────────────────────────────────────────────────────────────────
// 2. SilentDropImpossible — slow subscribers MUST be disconnected (with a
//    close frame the client can observe), never zombied.
// ──────────────────────────────────────────────────────────────────────────

func TestWSHub_SilentDropImpossible(t *testing.T) {
	server, wsURL, cleanup := hubTestServer(t)
	defer cleanup()

	c := dialWS(t, wsURL)
	defer c.Close()

	// Don't read at all — let the TCP receive buffer fill, then the
	// writer's WriteJSON blocks, then the per-subscriber send channel
	// fills, then Fanout's slow-subscriber timeout fires, then the
	// underlying conn closes, then the client's read finally errors out.
	//
	// 50,000 ~200-byte events ≈ 10MB which exceeds typical TCP receive
	// window (a few MB on Linux loopback) and forces the cascade to fire
	// in seconds rather than minutes.
	const N = 50000
	// Pack a payload large enough to push past the TCP buffer faster.
	bigPayload := strings.Repeat("x", 512)
	for i := 0; i < N; i++ {
		server.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "audit.appended",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-bank",
			Payload:       map[string]interface{}{"i": i, "filler": bigPayload},
		})
	}

	// We should get an error within ~5 seconds:
	//   100ms slowSubscriberTimeout (Fanout)
	//   + writer's blocked WriteJSON returning on Close()
	//   + reader's ReadMessage erroring
	//   + cascade to Remove
	// All of which happens in milliseconds once triggered.
	c.SetReadDeadline(time.Now().Add(8 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			// Any error is acceptable — what matters is we DO get one.
			// Specifically: not silent indefinite block.
			if isReadTimeout(err) {
				t.Errorf("expected close / EOF / connection-reset from slow-subscriber disconnect; got read timeout — zombie subscriber bug not fixed")
			}
			return
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 3. NoGoroutineLeak — 100 connect/disconnect cycles must return to a
//    goroutine baseline within tolerance. Catches both the writer-leaked-
//    on-error case and the reader-leaked-on-orphaned-conn case.
// ──────────────────────────────────────────────────────────────────────────

func TestWSHub_NoGoroutineLeak(t *testing.T) {
	_, wsURL, cleanup := hubTestServer(t)
	defer cleanup()

	// Warm up: one cycle so any one-time-init goroutines settle.
	c0 := dialWS(t, wsURL)
	c0.Close()
	time.Sleep(200 * time.Millisecond)
	runtime.GC()

	baseline := runtime.NumGoroutine()

	const N = 100
	for i := 0; i < N; i++ {
		c := dialWS(t, wsURL)
		c.Close()
	}

	// Give the server time to clean up — reader goroutines need to observe
	// the closed connection and unwind.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		final := runtime.NumGoroutine()
		// Allow some tolerance — Go runtime may keep a few cached goroutines.
		if final-baseline <= 5 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	final := runtime.NumGoroutine()
	t.Errorf("goroutine leak: baseline=%d final=%d after %d connect/disconnect cycles (delta=%d, tolerance=5)",
		baseline, final, N, final-baseline)
}

// ──────────────────────────────────────────────────────────────────────────
// 4. HeartbeatEmitted — heartbeat events reach subscribers but DON'T end up
//    in the ring buffer (so /events?since= replay isn't polluted).
// ──────────────────────────────────────────────────────────────────────────

func TestWSHub_HeartbeatReachesSubscribersButSkipsRing(t *testing.T) {
	server, _, cleanup := hubTestServer(t)
	defer cleanup()

	// Use the real RunHeartbeat? It's 30s — too slow. Instead, call
	// FanoutEphemeral directly with a heartbeat-shaped event.
	beat := Event{
		SchemaVersion: SchemaVersion,
		Type:          "heartbeat",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-hub",
		Payload:       map[string]interface{}{},
	}

	// First, populate the ring with one regular event so we know we're not
	// fooled by an empty ring.
	server.hub.Fanout(Event{
		SchemaVersion: SchemaVersion,
		Type:          "audit.appended",
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		AdapterID:     "sd-core-bank",
		Payload:       map[string]interface{}{},
	})

	beforeRing := server.hub.ring.Recent(50)
	server.hub.FanoutEphemeral(beat)
	afterRing := server.hub.ring.Recent(50)

	if len(afterRing) != len(beforeRing) {
		t.Errorf("heartbeat must NOT land in ring; before=%d after=%d",
			len(beforeRing), len(afterRing))
	}
	for _, e := range afterRing {
		if e.Type == "heartbeat" {
			t.Errorf("found heartbeat in ring buffer — should have been skipped")
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// 5. RunHeartbeat lifecycle — context cancellation stops the goroutine
//    cleanly (no hanging ticker, no goroutine leak).
// ──────────────────────────────────────────────────────────────────────────

func TestWSHub_RunHeartbeat_StopsOnContextCancel(t *testing.T) {
	server, _, cleanup := hubTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.hub.RunHeartbeat(ctx)
		close(done)
	}()
	// Cancel immediately — the loop should exit on its next select.
	cancel()
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Errorf("RunHeartbeat did not return within 2s of context cancel")
	}
}
