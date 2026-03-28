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

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
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
	"github.com/klauspost/compress/zstd"
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
// The server sends packets in the ubersdr hybrid binary format (see
// pcm_binary.go in the server source).  Two packet types:
//
//   Full header  (magic 0x5043 "PC", 29 bytes):
//     [0:2]  uint16  magic
//     [2]    uint8   version
//     [3]    uint8   format (0=PCM, 2=PCM-zstd)
//     [4:12] uint64  RTP timestamp (LE)
//     [12:20]uint64  wall-clock ms (LE)
//     [20:24]uint32  sample rate (LE)
//     [24]   uint8   channels
//     [25:29]uint32  reserved
//     [29:]  []byte  PCM samples (big-endian int16)
//
//   Version 2 full header (37 bytes) adds signal quality fields:
//     [25:29]float32 baseband power dBFS
//     [29:33]float32 noise density dBFS
//     [33:37]uint32  reserved
//     [37:]  []byte  PCM samples (big-endian int16)
//
//   Minimal header (magic 0x504D "PM", 13 bytes):
//     [0:2]  uint16  magic
//     [2]    uint8   version
//     [3:11] uint64  RTP timestamp (LE)
//     [11:13]uint16  reserved
//     [13:]  []byte  PCM samples (big-endian int16)

const (
	magicFull    = 0x5043 // "PC"
	magicMinimal = 0x504D // "PM"
)

type pcmDecoder struct {
	zd           *zstd.Decoder
	lastRate     int
	lastChannels int
}

func newPCMDecoder() (*pcmDecoder, error) {
	zd, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}
	return &pcmDecoder{zd: zd}, nil
}

// decode decompresses (if needed) and parses a binary PCM packet.
// Returns little-endian int16 PCM bytes, sample rate, channel count.
func (d *pcmDecoder) decode(data []byte, isZstd bool) ([]byte, int, int, error) {
	if isZstd {
		var err error
		data, err = d.zd.DecodeAll(data, nil)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("zstd decompress: %w", err)
		}
	}

	if len(data) < 4 {
		return nil, 0, 0, fmt.Errorf("packet too short (%d bytes)", len(data))
	}

	magic := binary.LittleEndian.Uint16(data[0:2])

	var rate, ch int
	var raw []byte

	switch magic {
	case magicFull:
		version := data[2]
		var headerLen int
		switch version {
		case 2:
			headerLen = 37
		default: // version 1
			headerLen = 29
		}
		if len(data) < headerLen {
			return nil, 0, 0, fmt.Errorf("full-header packet too short (%d < %d)", len(data), headerLen)
		}
		rate = int(binary.LittleEndian.Uint32(data[20:24]))
		ch = int(data[24])
		raw = data[headerLen:]
		d.lastRate = rate
		d.lastChannels = ch

	case magicMinimal:
		if len(data) < 13 {
			return nil, 0, 0, fmt.Errorf("minimal-header packet too short (%d bytes)", len(data))
		}
		raw = data[13:]
		rate = d.lastRate
		ch = d.lastChannels
		if rate == 0 || ch == 0 {
			return nil, 0, 0, fmt.Errorf("minimal header received before full header")
		}

	default:
		return nil, 0, 0, fmt.Errorf("unknown magic 0x%04X", magic)
	}

	// Convert big-endian int16 → little-endian int16
	n := len(raw) / 2
	le := make([]byte, len(raw))
	for i := 0; i < n; i++ {
		s := binary.BigEndian.Uint16(raw[i*2:])
		binary.LittleEndian.PutUint16(le[i*2:], s)
	}
	return le, rate, ch, nil
}

func (d *pcmDecoder) close() { d.zd.Close() }

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
	running       bool
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
	q.Set("format", "pcm-zstd")
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
				c.running = false
				return false
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
// main
// ---------------------------------------------------------------------------

func main() {
	var (
		rawURL   = flag.String("url", "", "UberSDR base URL, e.g. http://host:8080 (required)")
		freq     = flag.Int("freq", 0, "Centre frequency in Hz (required)")
		iqMode   = flag.String("iq-mode", "iq", "IQ mode: iq (10 kHz), iq48, iq96, iq192, iq384")
		pass     = flag.String("pass", "", "Bypass password (optional)")
		noReconn = flag.Bool("no-reconnect", false, "Disable auto-reconnect on disconnect")
	)
	flag.Parse()

	// Validate iq-mode
	modeInfo, modeOK := iqModes[*iqMode]
	if !modeOK {
		fmt.Fprintf(os.Stderr, "error: unknown -iq-mode %q (valid: iq, iq48, iq96, iq192, iq384)\n", *iqMode)
		os.Exit(1)
	}

	if *rawURL == "" || *freq == 0 {
		fmt.Fprintf(os.Stderr, "Usage: ubersdr_iq -url <http://host:port> -freq <Hz> [-iq-mode <mode>] [-pass <password>] [-no-reconnect]\n\n")
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

	c := &client{
		baseURL:       *rawURL,
		frequency:     *freq,
		iqMode:        *iqMode,
		password:      *pass,
		sessionID:     uuid.New().String(),
		autoReconnect: !*noReconn,
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
