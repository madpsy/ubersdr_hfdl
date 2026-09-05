// ubersdr_iq - Minimal UberSDR IQ stream client for dumphfdl
//
// Connects to an UberSDR instance, requests an IQ mode centred on the given
// frequency and writes a continuous stream of raw CS16 (little-endian signed
// 16-bit interleaved I/Q) samples to stdout.
//
// Pipe directly into dumphfdl:
//
//	ubersdr_iq -url http://sdr.example.com:8080 -freq 10081000 | \
//	  dumphfdl --iq-file - --sample-format CS16 --sample-rate 10000 \
//	           --centerfreq 0 0
//
// For wider bandwidth (e.g. iq48 = 48 kHz, covering multiple HFDL channels):
//
//	ubersdr_iq -url http://sdr.example.com:8080 -freq 10063000 -iq-mode iq48 | \
//	  dumphfdl --iq-file - --sample-format CS16 --sample-rate 48000 \
//	           --centerfreq 10063 10063 10081 10084
//
// Usage:
//
//	ubersdr_iq [flags]
//	  -url     string   UberSDR base URL, e.g. http://host:8080  (required)
//	  -freq    int      Centre frequency in Hz                    (required)
//	  -iq-mode string   IQ mode: iq, iq48, iq96, iq192, iq384    (default: iq)
//	  -pass    string   Bypass password (optional)
//	  -no-reconnect     Disable auto-reconnect on disconnect
//	  -min-margin float Reduced-depth IQ, in dB of margin under the noise floor
//	                    (15-60, default 26; 0 = lossless)

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/szpajder/dumphfdl/ubersdr_iq/internal/pcmv4"
)

const rcvBufSize = 16 * 1024 * 1024 // 16 MiB SO_RCVBUF for the IQ WebSocket connection

// wsDialer is a websocket.Dialer that sets SO_RCVBUF = 16 MiB on the
// underlying TCP socket before the WebSocket handshake.
var wsDialer = &websocket.Dialer{
	HandshakeTimeout: 10 * time.Second,
	NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		nd := &net.Dialer{}
		conn, err := nd.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			raw, err := tc.SyscallConn()
			if err == nil {
				_ = raw.Control(func(fd uintptr) {
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBufSize)
				})
			}
		}
		return conn, nil
	},
}

// ---------------------------------------------------------------------------
// IQ mode table
// ---------------------------------------------------------------------------

// iqModeInfo holds the properties of each supported IQ mode.
type iqModeInfo struct {
	sampleRate int // samples per second delivered by the server
}

// iqModes maps mode name → properties.
var iqModes = map[string]iqModeInfo{
	"iq":    {sampleRate: 10000},
	"iq48":  {sampleRate: 48000},
	"iq96":  {sampleRate: 96000},
	"iq192": {sampleRate: 192000},
	"iq384": {sampleRate: 384000},
}

// ---------------------------------------------------------------------------
// Protocol types (mirrors the ubersdr server protocol)
// ---------------------------------------------------------------------------

type connectionCheckRequest struct {
	UserSessionID string `json:"user_session_id"`
	Password      string `json:"password,omitempty"`
}

type connectionCheckResponse struct {
	Allowed        bool     `json:"allowed"`
	Reason         string   `json:"reason,omitempty"`
	ClientIP       string   `json:"client_ip,omitempty"`
	Bypassed       bool     `json:"bypassed"`
	AllowedIQModes []string `json:"allowed_iq_modes,omitempty"`
	MaxSessionTime int      `json:"max_session_time"`
}

// wsMessage covers the JSON messages the server may send.
type wsMessage struct {
	Type      string `json:"type"`
	Error     string `json:"error,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Frequency int    `json:"frequency,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// ---------------------------------------------------------------------------
// PCM binary packet decoder
// ---------------------------------------------------------------------------
// Audio protocol version 4 only. The wire format lives in pcm_v4_header.go and
// pcm_predictive.go beside this file; versions 1 to 3 -- a zstd frame wrapping a
// fixed 29- or 37-byte header, samples big-endian and verbatim -- are gone, and
// the server refuses a version it cannot serve rather than quietly serving an
// older one.

type pcmDecoder struct {
	v4 *pcmv4.PCMv4StreamDecoder
}

// newPCMDecoder returns a decoder for one connection.
//
// One per connection is the requirement, not a convenience: the predictor's taps
// are derived from the samples already decoded, so carrying a decoder across a
// reconnect would decode the new stream against the old one's filter state and
// produce plausible noise rather than an error. runOnce() builds one per
// session, which satisfies that.
func newPCMDecoder() (*pcmDecoder, error) {
	return &pcmDecoder{v4: pcmv4.NewPCMv4StreamDecoder()}, nil
}

// decode parses one version 4 packet.
//
// Returns little-endian int16 PCM bytes, sample rate, channel count -- the same
// shape the versions 1-3 decoder returned, so the caller is unchanged. Unlike
// that one there is no byte swap: versions 1-3 carried radiod's big-endian
// samples verbatim, while version 4 carries int16 values that the codec renders
// little-endian directly, which is what CS16 on stdout wants anyway.
func (d *pcmDecoder) decode(data []byte, _ bool) ([]byte, int, int, error) {
	// A server older than 0.1.63 clamps a version it cannot serve down to 1 and
	// answers with a zstd frame rather than refusing. Naming that beats a bad
	// magic reported once per packet.
	if pcmv4.IsZstdFrame(data) {
		return nil, 0, 0, fmt.Errorf(
			"server does not support audio protocol version %d (needs UberSDR 0.1.63 or later)",
			pcmv4.ProtocolVersion)
	}

	pcmLE, rate, ch, _, _, err := d.v4.DecodePacketLE(data)
	if err != nil {
		return nil, 0, 0, err
	}
	return pcmLE, rate, ch, nil
}

func (d *pcmDecoder) close() {}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type client struct {
	baseURL       string // e.g. "http://host:8080"
	frequency     int
	iqMode        string // "iq", "iq48", "iq96", "iq192", "iq384"
	password      string
	sessionID     string
	autoReconnect bool

	// minMargin is the reduced-depth IQ request in dB, or 0 for the lossless
	// stream. On by default at minMarginDefaultDB; only -min-margin 0 turns it
	// off. See parseMinMargin.
	minMargin int

	running bool
}

// httpBase returns the http(s) base URL derived from the user-supplied URL.
func (c *client) httpBase() string {
	u, _ := url.Parse(c.baseURL)
	// Accept http/https/ws/wss as input scheme
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host)
}

// wsURL builds the WebSocket endpoint URL.
func (c *client) wsURL() string {
	u, _ := url.Parse(c.baseURL)
	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}

	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/ws"
	}

	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d", c.frequency))
	q.Set("mode", c.iqMode)
	// The format name is historical: it selects the lossless path, which from
	// version 4 is the predictive codec rather than a zstd wrapper.
	q.Set("format", "pcm-zstd")
	// Named explicitly rather than left to the server's default of 1.
	q.Set("version", fmt.Sprintf("%d", pcmv4.ProtocolVersion))
	// Sent unless -min-margin 0 turned it off. An absent min_margin is not the
	// same thing to the server as a zero one: absent is the lossless path,
	// which is what asking for 0 has to produce, and a server too old to know
	// the parameter ignores it and sends the lossless stream anyway.
	if c.minMargin > 0 {
		q.Set("min_margin", fmt.Sprintf("%d", c.minMargin))
	}
	q.Set("user_session_id", c.sessionID)
	if c.password != "" {
		q.Set("password", c.password)
	}

	return fmt.Sprintf("%s://%s%s?%s", wsScheme, u.Host, path, q.Encode())
}

// checkConnection calls /connection and returns whether we are allowed.
func (c *client) checkConnection() (bool, error) {
	endpoint := c.httpBase() + "/connection"

	body, _ := json.Marshal(connectionCheckRequest{
		UserSessionID: c.sessionID,
		Password:      c.password,
	})

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ubersdr_hfdl/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Server unreachable — try anyway (matches ubersdr client behaviour)
		fmt.Fprintf(os.Stderr, "connection check failed (%v), attempting anyway\n", err)
		return true, nil
	}
	defer resp.Body.Close()

	var cr connectionCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return false, fmt.Errorf("decode /connection response: %w", err)
	}

	if !cr.Allowed {
		return false, fmt.Errorf("server rejected connection: %s", cr.Reason)
	}

	fmt.Fprintf(os.Stderr, "connection allowed (IP: %s, bypassed: %v, max session: %ds)\n",
		cr.ClientIP, cr.Bypassed, cr.MaxSessionTime)
	return true, nil
}

// runOnce performs one connection attempt.  Returns true if the caller should
// reconnect, false if it should exit cleanly.
func (c *client) runOnce() (reconnect bool) {
	// Generate a fresh UUID for every connection attempt so the UberSDR server
	// never sees the same session ID twice (even across reconnects).
	c.sessionID = uuid.New().String()

	allowed, err := c.checkConnection()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return c.autoReconnect
	}
	if !allowed {
		return false
	}

	wsAddr := c.wsURL()
	fmt.Fprintf(os.Stderr, "connecting to %s\n", wsAddr)

	hdr := http.Header{}
	hdr.Set("User-Agent", "ubersdr_hfdl/1.0")
	conn, _, err := wsDialer.Dial(wsAddr, hdr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "websocket dial: %v\n", err)
		return c.autoReconnect
	}
	defer conn.Close()

	info := iqModes[c.iqMode]
	fmt.Fprintf(os.Stderr, "connected — mode=%s, centre=%d Hz, expected sample rate=%d Hz\n",
		c.iqMode, c.frequency, info.sampleRate)

	dec, err := newPCMDecoder()
	if err != nil {
		fmt.Fprintf(os.Stderr, "decoder init: %v\n", err)
		return false
	}
	defer dec.close()

	// Keepalive goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
					fmt.Fprintf(os.Stderr, "keepalive error: %v\n", err)
					return
				}
			}
		}
	}()

	firstPacket := true

	for c.running {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				fmt.Fprintf(os.Stderr, "server closed connection\n")
			} else {
				fmt.Fprintf(os.Stderr, "read error: %v\n", err)
			}
			return c.autoReconnect
		}

		switch msgType {
		case websocket.BinaryMessage:
			pcm, rate, ch, err := dec.decode(msg, true /* pcm-zstd */)
			if err != nil {
				fmt.Fprintf(os.Stderr, "decode error: %v\n", err)
				continue
			}
			if len(pcm) == 0 {
				continue
			}
			if firstPacket {
				fmt.Fprintf(os.Stderr, "receiving IQ: %d Hz, %d channel(s)\n", rate, ch)
				firstPacket = false
			}
			// Write raw CS16 to stdout
			if _, err := os.Stdout.Write(pcm); err != nil {
				fmt.Fprintf(os.Stderr, "stdout write error: %v\n", err)
				return false
			}

		case websocket.TextMessage:
			var m wsMessage
			if err := json.Unmarshal(msg, &m); err != nil {
				fmt.Fprintf(os.Stderr, "json parse: %v\n", err)
				continue
			}
			switch m.Type {
			case "error":
				fmt.Fprintf(os.Stderr, "server error: %s\n", m.Error)
				return c.autoReconnect
			case "status":
				fmt.Fprintf(os.Stderr, "status: session=%s freq=%d mode=%s\n",
					m.SessionID, m.Frequency, m.Mode)
			case "pong":
				// keepalive ack — ignore
			}
		}
	}

	return false
}

// run is the top-level loop with optional exponential-backoff reconnect.
func (c *client) run() int {
	retries := 0
	maxBackoff := 60 * time.Second

	for {
		reconnect := c.runOnce()
		if !reconnect || !c.running {
			return 0
		}

		retries++
		backoff := time.Duration(1<<uint(retries)) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		fmt.Fprintf(os.Stderr, "reconnecting in %.0fs (attempt %d)…\n", backoff.Seconds(), retries)

		select {
		case <-time.After(backoff):
		case <-func() <-chan struct{} {
			ch := make(chan struct{})
			go func() {
				for c.running {
					time.Sleep(100 * time.Millisecond)
				}
				close(ch)
			}()
			return ch
		}():
			return 0
		}
	}
}

// ---------------------------------------------------------------------------
// Reduced-depth IQ
// ---------------------------------------------------------------------------

// What -min-margin may be set to. These are the server's own limits, from
// lossyMinMarginDB and lossyMaxMarginDB in pcm_lossy.go, repeated here so a
// value outside them is refused with a reason at startup rather than silently
// clamped to a different one for the life of the process.
//
// The floor is where the quantisation noise starts to lift the noise floor a
// client can see: 15 dB down adds 0.14 dB to it, under what a receiver's own
// readings resolve, where 6 dB down adds a full 1 dB. Above 60 dB the request
// buys nothing measurable.
const (
	minMarginMinDB = 15.0
	minMarginMaxDB = 60.0
)

// What -min-margin is when nobody says otherwise, and it is on: 26 dB, the same
// MARGIN_DEFAULT_DB the UberSDR web client uses, the measured transparent
// setting where every FT8 decode survives with its reported strength intact. It
// costs about 0.01 dB of noise floor for roughly half the bytes on a stream that
// never stops, and the margin sits far under what dumphfdl needs to demodulate
// an HFDL burst. -min-margin 0 asks for the lossless stream instead.
const minMarginDefaultDB = 26

// parseMinMargin validates the -min-margin flag and returns the whole dB the
// server will be asked for.
//
// Strict on purpose. The server clamps whatever it is sent into its own range
// and rounds it to a whole dB, so a value outside the range would produce a
// working but different stream and nothing downstream would ever say so -- a
// launcher reading dumphfdl's output would see fewer frames and no reason for
// it. Zero is the one value accepted outside the range: it is how a command line
// turns the default off and asks for the lossless stream.
func parseMinMargin(v float64) (int, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%v is not a number of dB", v)
	}
	if v == 0 {
		return 0, nil
	}
	if v < minMarginMinDB || v > minMarginMaxDB {
		return 0, fmt.Errorf("%g dB is outside %g-%g; the server would not honour it as asked. "+
			"Pass 0 for a lossless stream", v, minMarginMinDB, minMarginMaxDB)
	}
	dB := int(math.Round(v))
	if float64(dB) != v {
		fmt.Fprintf(os.Stderr, "-min-margin: %g dB rounded to %d, which is what the server would have done with it\n", v, dB)
	}
	return dB, nil
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	var (
		rawURL    = flag.String("url", "", "UberSDR base URL, e.g. http://host:8080 (required)")
		freq      = flag.Int("freq", 0, "Centre frequency in Hz (required)")
		iqMode    = flag.String("iq-mode", "iq", "IQ mode: iq (10 kHz), iq48, iq96, iq192, iq384")
		pass      = flag.String("pass", "", "Bypass password (optional)")
		noReconn  = flag.Bool("no-reconnect", false, "Disable auto-reconnect on disconnect")
		minMargin = flag.Float64("min-margin", minMarginDefaultDB,
			"Reduced-depth IQ: dB of margin under the noise floor (15-60; 0 = lossless)")
	)
	flag.Parse()

	marginDB, err := parseMinMargin(*minMargin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -min-margin: %v\n", err)
		os.Exit(1)
	}

	// Validate iq-mode
	modeInfo, modeOK := iqModes[*iqMode]
	if !modeOK {
		fmt.Fprintf(os.Stderr, "error: unknown -iq-mode %q (valid: iq, iq48, iq96, iq192, iq384)\n", *iqMode)
		os.Exit(1)
	}

	if *rawURL == "" || *freq == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ubersdr_iq -url <http://host:port> -freq <Hz> [-iq-mode <mode>] [-pass <password>] [-no-reconnect] [-min-margin <dB>]\n\n")
		fmt.Fprintf(os.Stderr, "IQ modes and their sample rates:\n")
		fmt.Fprintf(os.Stderr, "  iq    — 10,000 Hz  (10 kHz bandwidth,  1 HFDL channel)\n")
		fmt.Fprintf(os.Stderr, "  iq48  — 48,000 Hz  (48 kHz bandwidth,  ~5 channels)\n")
		fmt.Fprintf(os.Stderr, "  iq96  — 96,000 Hz  (96 kHz bandwidth, ~10 channels)\n")
		fmt.Fprintf(os.Stderr, "  iq192 — 192,000 Hz (192 kHz bandwidth)\n")
		fmt.Fprintf(os.Stderr, "  iq384 — 384,000 Hz (384 kHz bandwidth)\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  # Single channel (iq, 10 kHz):\n")
		fmt.Fprintf(os.Stderr, "  ubersdr_iq -url http://host:8080 -freq 10081000 | \\\n")
		fmt.Fprintf(os.Stderr, "    dumphfdl --iq-file - --sample-format CS16 --sample-rate 10000 --centerfreq 0 0\n\n")
		fmt.Fprintf(os.Stderr, "  # Multi-channel (iq48, 48 kHz):\n")
		fmt.Fprintf(os.Stderr, "  ubersdr_iq -url http://host:8080 -freq 10063000 -iq-mode iq48 | \\\n")
		fmt.Fprintf(os.Stderr, "    dumphfdl --iq-file - --sample-format CS16 --sample-rate 48000 --centerfreq 10063 10063 10081 10084\n")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "mode=%s  sample-rate=%d Hz\n", *iqMode, modeInfo.sampleRate)

	// Said either way, because the effect is invisible from dumphfdl's side:
	// the same samples arrive at the same rate, and only the link to the
	// receiver carries fewer bytes for them.
	if marginDB == minMarginDefaultDB {
		fmt.Fprintf(os.Stderr, "reduced-depth IQ: %d dB of margin under the noise floor "+
			"(the default; -min-margin 0 for the lossless stream)\n", marginDB)
	} else if marginDB > 0 {
		fmt.Fprintf(os.Stderr, "reduced-depth IQ: %d dB of margin under the noise floor\n", marginDB)
	} else {
		fmt.Fprintf(os.Stderr, "reduced-depth IQ off: taking the lossless stream\n")
	}

	c := &client{
		baseURL:       *rawURL,
		frequency:     *freq,
		iqMode:        *iqMode,
		password:      *pass,
		sessionID:     uuid.New().String(),
		autoReconnect: !*noReconn,
		minMargin:     marginDB,
		running:       true,
	}

	// Handle SIGINT / SIGTERM gracefully
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Fprintf(os.Stderr, "\nshutting down\n")
		c.running = false
	}()

	os.Exit(c.run())
}
