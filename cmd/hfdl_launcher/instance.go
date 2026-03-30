package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// instance represents one running ubersdr_iq | dumphfdl pipeline.
type instance struct {
	group           freqGroup
	ubersdrPath     string
	dumphfdlPath    string
	ubersdrURL      string
	password        string
	systemTable     string
	extraHFDLArgs   []string
	jsonCh          chan<- string // fan-in channel for JSON lines from dumphfdl stdout
	iqRecordDir     string        // directory for IQ WAV recordings; empty = disabled
	iqRecordSeconds int           // duration of each recording in seconds (default 30)

	mu            sync.Mutex
	running       bool
	stopping      bool
	startedAt     time.Time // when the current run started (zero = not running)
	lastHealthyAt time.Time // last time the pipeline was confirmed running (zero = never)
	reconnections int       // number of automatic restarts since launch
}

// buildArgs returns the argument lists for ubersdr_iq and dumphfdl.
//
// --output decoded:json:file:path=- is always injected unconditionally so the
// launcher can read decoded messages for the web stats server.  It is placed
// after any user-supplied extra args and before the channel frequencies (which
// must be last per dumphfdl's CLI convention).
func (inst *instance) buildArgs() (iqArgs []string, hfdlArgs []string) {
	info := iqModes[inst.group.iqMode]

	// ubersdr_iq args
	iqArgs = []string{
		"-url", inst.ubersdrURL,
		"-freq", strconv.Itoa(inst.group.centerKHz * 1000),
		"-iq-mode", inst.group.iqMode,
		"-no-reconnect", // launcher handles reconnect
	}
	if inst.password != "" {
		iqArgs = append(iqArgs, "-pass", inst.password)
	}

	// dumphfdl args: fixed IQ input params first
	hfdlArgs = []string{
		"--iq-file", "-",
		"--sample-format", "CS16",
		"--sample-rate", strconv.Itoa(info.sampleRateHz),
		"--centerfreq", strconv.Itoa(inst.group.centerKHz),
	}
	if inst.systemTable != "" {
		hfdlArgs = append(hfdlArgs, "--system-table", inst.systemTable)
	}

	// User-supplied extra args
	hfdlArgs = append(hfdlArgs, inst.extraHFDLArgs...)

	// Always inject JSON stdout output for the internal stats server.
	// Must come before channel frequencies (which must be last).
	hfdlArgs = append(hfdlArgs, "--output", "decoded:json:file:path=-")

	// Channel frequencies must be last
	for _, f := range inst.group.freqsKHz {
		hfdlArgs = append(hfdlArgs, strconv.Itoa(f))
	}

	return
}

// start launches the pipeline.  ubersdr_iq stdout → dumphfdl stdin.
// dumphfdl stdout is captured line-by-line and forwarded to the fan-in channel.
// Both processes share the current process's stderr for log output.
//
// When iqRecordDir is non-empty, the raw IQ stream is teed into a WAV file for
// iqRecordSeconds seconds before being forwarded to dumphfdl unmodified.
func (inst *instance) start() error {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	iqArgs, hfdlArgs := inst.buildArgs()

	log.Printf("[%d kHz / %s] starting: %s %s | %s %s",
		inst.group.centerKHz, inst.group.iqMode,
		inst.ubersdrPath, strings.Join(iqArgs, " "),
		inst.dumphfdlPath, strings.Join(hfdlArgs, " "))

	// Build ubersdr_iq command
	iqCmd := exec.Command(inst.ubersdrPath, iqArgs...)
	iqCmd.Stderr = os.Stderr

	// Build dumphfdl command
	hfdlCmd := exec.Command(inst.dumphfdlPath, hfdlArgs...)
	hfdlCmd.Stderr = os.Stderr

	// Capture dumphfdl stdout via pipe for JSON fan-in
	hfdlStdout, err := hfdlCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("dumphfdl stdout pipe: %w", err)
	}

	// Obtain ubersdr_iq stdout pipe
	iqPipe, err := iqCmd.StdoutPipe()
	if err != nil {
		hfdlStdout.Close()
		return fmt.Errorf("ubersdr_iq stdout pipe: %w", err)
	}

	if inst.iqRecordDir != "" {
		// IQ recording enabled: interpose a pipe between ubersdr_iq and dumphfdl.
		// recordIQ tees the stream to a WAV file for iqRecordSeconds, then passes
		// the remainder through to dumphfdl stdin unmodified.
		pr, pw := io.Pipe()
		hfdlCmd.Stdin = pr

		// Start dumphfdl first so it is ready to receive
		if err := hfdlCmd.Start(); err != nil {
			iqPipe.Close()
			hfdlStdout.Close()
			pr.Close()
			pw.Close()
			return fmt.Errorf("start dumphfdl: %w", err)
		}

		// Start ubersdr_iq
		if err := iqCmd.Start(); err != nil {
			hfdlCmd.Process.Kill()
			hfdlStdout.Close()
			pr.Close()
			pw.Close()
			return fmt.Errorf("start ubersdr_iq: %w", err)
		}

		inst.running = true
		inst.startedAt = time.Now()
		inst.lastHealthyAt = time.Now()

		secs := inst.iqRecordSeconds
		if secs <= 0 {
			secs = 30
		}
		info := iqModes[inst.group.iqMode]
		go func() {
			// recordIQ reads from iqPipe, writes WAV for secs, then copies the
			// rest to pw (dumphfdl stdin).  Closing pw signals EOF to dumphfdl.
			recordIQ(iqPipe, pw, inst.iqRecordDir,
				inst.group.centerKHz, inst.group.iqMode,
				info.sampleRateHz, time.Duration(secs)*time.Second)
			pw.Close()
		}()
	} else {
		// No recording: connect ubersdr_iq stdout directly to dumphfdl stdin.
		hfdlCmd.Stdin = iqPipe

		// Start dumphfdl first so it is ready to receive
		if err := hfdlCmd.Start(); err != nil {
			iqPipe.Close()
			hfdlStdout.Close()
			return fmt.Errorf("start dumphfdl: %w", err)
		}

		// Start ubersdr_iq
		if err := iqCmd.Start(); err != nil {
			hfdlCmd.Process.Kill()
			hfdlStdout.Close()
			return fmt.Errorf("start ubersdr_iq: %w", err)
		}

		inst.running = true
		inst.startedAt = time.Now()
		inst.lastHealthyAt = time.Now()
	}

	// Read dumphfdl stdout line-by-line and forward to the fan-in channel.
	go func() {
		scanner := bufio.NewScanner(hfdlStdout)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" && inst.jsonCh != nil {
				select {
				case inst.jsonCh <- line:
				default:
					// channel full — drop rather than block
				}
			}
		}
	}()

	// Monitor: when ubersdr_iq exits, tear down dumphfdl and schedule restart.
	go func() {
		iqErr := iqCmd.Wait()
		iqPipe.Close()
		// Give dumphfdl a moment to flush, then kill it
		time.Sleep(500 * time.Millisecond)
		if hfdlCmd.Process != nil {
			hfdlCmd.Process.Kill()
		}
		hfdlCmd.Wait()

		inst.mu.Lock()
		inst.running = false
		inst.startedAt = time.Time{} // zero = not running
		shouldRestart := !inst.stopping
		inst.mu.Unlock()

		if iqErr != nil {
			log.Printf("[%d kHz / %s] ubersdr_iq exited: %v", inst.group.centerKHz, inst.group.iqMode, iqErr)
		} else {
			log.Printf("[%d kHz / %s] pipeline exited", inst.group.centerKHz, inst.group.iqMode)
		}

		if shouldRestart {
			log.Printf("[%d kHz / %s] restarting in 10s…", inst.group.centerKHz, inst.group.iqMode)
			time.Sleep(10 * time.Second)
			inst.mu.Lock()
			stillStopping := inst.stopping
			if !stillStopping {
				inst.reconnections++
			}
			inst.mu.Unlock()
			if !stillStopping {
				if err := inst.start(); err != nil {
					log.Printf("[%d kHz / %s] restart failed: %v", inst.group.centerKHz, inst.group.iqMode, err)
				}
			}
		}
	}()

	return nil
}

// stop marks the instance as intentionally stopped, suppressing auto-restart.
func (inst *instance) stop() {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.stopping = true
}
