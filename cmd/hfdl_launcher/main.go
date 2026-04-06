// hfdl_launcher - Automatic multi-instance HFDL decoder launcher
//
// Fetches the current HFDL ground station frequency table, groups frequencies
// into IQ windows, and launches one ubersdr_iq | dumphfdl pipeline per window.
//
// The launcher always injects --output decoded:json:file:path=- into every
// dumphfdl invocation so decoded messages are streamed to the built-in web
// statistics server.  Users do not need to add this themselves.
//
// Usage:
//
//	hfdl_launcher [flags] [-- dumphfdl-extra-args...]
//
//	  -url               UberSDR base URL (default: http://ubersdr:8080)
//	  -pass              Bypass password (optional)
//	  -ubersdr-iq        Path to ubersdr_iq binary (default: ubersdr_iq)
//	  -dumphfdl          Path to dumphfdl binary (default: dumphfdl)
//	  -freq-url          URL for HFDL frequency JSONL
//	  -station           Comma-separated ground station IDs to monitor (default: all)
//	  -system-table      Path to dumphfdl system table file (optional)
//	  -config-pass       Password required to use the Apply endpoints (optional)
//	  -dry-run           Print planned instances without launching
//	  -web-port          Port for the web statistics server (default: 8080, 0 = disabled)
//	  -web-static        Path to static web files directory
//	  -iq-record-dir     Directory to write IQ WAV recordings (enables recording when set)
//	  -iq-record-seconds Duration of each IQ recording in seconds (default: 30)

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: hfdl_launcher [flags] [-- dumphfdl-args...]\n\n")                                                 //nolint:errcheck
	fmt.Fprintf(w, "Flags:\n")                                                                                               //nolint:errcheck
	fmt.Fprintf(w, "  -url               UberSDR base URL (default: http://ubersdr:8080)\n")                                 //nolint:errcheck
	fmt.Fprintf(w, "  -pass              Bypass password (optional)\n")                                                      //nolint:errcheck
	fmt.Fprintf(w, "  -ubersdr-iq        Path to ubersdr_iq binary (default: ubersdr_iq)\n")                                 //nolint:errcheck
	fmt.Fprintf(w, "  -dumphfdl          Path to dumphfdl binary (default: dumphfdl)\n")                                     //nolint:errcheck
	fmt.Fprintf(w, "  -freq-url          URL for HFDL frequency JSONL\n")                                                    //nolint:errcheck
	fmt.Fprintf(w, "                     (default: https://ubersdr.org/hfdl/hfdl_frequencies.jsonl)\n")                      //nolint:errcheck
	fmt.Fprintf(w, "  -station           Comma-separated ground station IDs to monitor (default: all)\n")                    //nolint:errcheck
	fmt.Fprintf(w, "  -system-table      Path to dumphfdl system table file (optional)\n")                                   //nolint:errcheck
	fmt.Fprintf(w, "  -config-pass       Password to protect the Apply frequency endpoints (optional)\n")                    //nolint:errcheck
	fmt.Fprintf(w, "  -dry-run           Print planned instances without launching\n")                                       //nolint:errcheck
	fmt.Fprintf(w, "  -web-port          Port for the web statistics server (default: 6090, 0 = disabled)\n")                //nolint:errcheck
	fmt.Fprintf(w, "  -web-static        Path to static web files directory\n")                                              //nolint:errcheck
	fmt.Fprintf(w, "                     (default: /usr/local/share/hfdl_launcher/static)\n")                                //nolint:errcheck
	fmt.Fprintf(w, "  -iq-record-dir     Directory to write IQ WAV recordings (enables recording when set)\n")               //nolint:errcheck
	fmt.Fprintf(w, "  -iq-record-seconds Duration of each IQ recording in seconds (default: 30)\n\n")                        //nolint:errcheck
	fmt.Fprintf(w, "Extra dumphfdl arguments:\n")                                                                            //nolint:errcheck
	fmt.Fprintf(w, "  Any arguments after -- are passed verbatim to every dumphfdl instance.\n")                             //nolint:errcheck
	fmt.Fprintf(w, "  Note: --output decoded:json:file:path=- is always injected automatically.\n\n")                        //nolint:errcheck
	fmt.Fprintf(w, "Bandwidth selection:\n")                                                                                 //nolint:errcheck
	fmt.Fprintf(w, "  iq48  — 48 kHz  (channels clustered within 48 kHz)\n")                                                 //nolint:errcheck
	fmt.Fprintf(w, "  iq96  — 96 kHz  (channels spanning 48–96 kHz)\n")                                                      //nolint:errcheck
	fmt.Fprintf(w, "  iq192 — 192 kHz (channels spanning 96–192 kHz)\n")                                                     //nolint:errcheck
	fmt.Fprintf(w, "  Clusters wider than 192 kHz are split across multiple windows.\n\n")                                   //nolint:errcheck
	fmt.Fprintf(w, "Examples:\n")                                                                                            //nolint:errcheck
	fmt.Fprintf(w, "  hfdl_launcher -url http://sdr.example.com:8080\n")                                                     //nolint:errcheck
	fmt.Fprintf(w, "  hfdl_launcher -url http://sdr.example.com:8080 -station 1,2,3\n")                                      //nolint:errcheck
	fmt.Fprintf(w, "  hfdl_launcher -url http://sdr.example.com:8080 -- --output decoded:json:tcp:address=host,port=5555\n") //nolint:errcheck
}

func run(cfg config) error {
	if len(cfg.stationIDs) > 0 {
		ids := make([]int, 0, len(cfg.stationIDs))
		for id := range cfg.stationIDs {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		log.Printf("filtering to station IDs: %v", ids)
	}

	log.Printf("fetching HFDL frequencies from %s", cfg.freqURL)
	fetched, err := fetchFrequencies(cfg.freqURL, cfg.stationIDs)
	if err != nil {
		return fmt.Errorf("fetch frequencies: %w", err)
	}
	if len(fetched.Freqs) == 0 {
		return fmt.Errorf("no frequencies found (check -station IDs are valid)")
	}
	log.Printf("found %d unique HFDL frequencies (%d – %d kHz)",
		len(fetched.Freqs), fetched.Freqs[0], fetched.Freqs[len(fetched.Freqs)-1])

	groups := groupFrequencies(fetched.Freqs)
	log.Printf("grouped into %d windows (auto-selected bandwidths)", len(groups))
	for i, g := range groups {
		log.Printf("  window %2d: centre=%d kHz  mode=%s  channels=%v",
			i+1, g.centerKHz, g.iqMode, g.freqsKHz)
	}

	if cfg.dryRun {
		log.Printf("dry-run commands:")
		for _, g := range groups {
			inst := &instance{
				group:         g,
				ubersdrPath:   cfg.ubersdrPath,
				dumphfdlPath:  cfg.dumphfdlPath,
				ubersdrURL:    cfg.ubersdrURL,
				password:      cfg.password,
				systemTable:   cfg.systemTable,
				extraHFDLArgs: cfg.extraHFDLArgs,
			}
			iqArgs, hfdlArgs := inst.buildArgs()
			log.Printf("  %s %s | %s %s",
				cfg.ubersdrPath, strings.Join(iqArgs, " "),
				cfg.dumphfdlPath, strings.Join(hfdlArgs, " "))
		}
		return nil
	}

	// Fan-in channel: all dumphfdl instances write JSON lines here.
	jsonCh := make(chan string, 1024)

	// Stats store + ingestion goroutine.
	store := newStatsStore(fetched.GSNames, fetched.FreqGSID, fetched.Stations)
	go func() {
		for line := range jsonCh {
			store.ingest(line)
		}
	}()
	go store.purgeStaleAircraft()

	// exitCh: Apply endpoints send on this to trigger a clean exit so Docker
	// restarts the container and re-reads the updated frequency file.
	exitCh := make(chan struct{}, 1)

	// Ensure IQ recording directory exists when recording is enabled.
	if cfg.iqRecordDir != "" {
		if err := os.MkdirAll(cfg.iqRecordDir, 0o755); err != nil {
			log.Printf("warning: cannot create IQ recording directory %s: %v — recording disabled", cfg.iqRecordDir, err)
			cfg.iqRecordDir = ""
		} else {
			log.Printf("IQ recording enabled: directory=%s duration=%ds", cfg.iqRecordDir, cfg.iqRecordSeconds)
		}
	}

	// Build instances (not yet started).
	instances := make([]*instance, len(groups))
	for i, g := range groups {
		instances[i] = &instance{
			group:           g,
			ubersdrPath:     cfg.ubersdrPath,
			dumphfdlPath:    cfg.dumphfdlPath,
			ubersdrURL:      cfg.ubersdrURL,
			password:        cfg.password,
			systemTable:     cfg.systemTable,
			extraHFDLArgs:   cfg.extraHFDLArgs,
			jsonCh:          jsonCh,
			iqRecordDir:     cfg.iqRecordDir,
			iqRecordSeconds: cfg.iqRecordSeconds,
		}
	}

	// Web server — started after instances are built so the handler can read
	// live health fields from each *instance at request time.
	if cfg.webPort > 0 {
		go startWebServer(cfg.webPort, cfg.webStaticDir, store, instances, groups, fetched.DisabledFreqs, cfg.extraHFDLArgs, cfg.freqURL, cfg.configPass, cfg.ubersdrURL, exitCh)
	}

	for _, inst := range instances {
		if err := inst.start(); err != nil {
			log.Printf("warning: failed to start instance for %d kHz (%s): %v",
				inst.group.centerKHz, inst.group.iqMode, err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("all instances started — press Ctrl+C to stop")

	select {
	case <-sigs:
		log.Printf("shutting down all instances…")
	case <-exitCh:
		log.Printf("frequency config updated — exiting for restart…")
	}

	for _, inst := range instances {
		inst.stop()
	}
	time.Sleep(2 * time.Second)
	return nil
}

// config holds all launcher configuration.
type config struct {
	ubersdrURL      string
	password        string
	ubersdrPath     string
	dumphfdlPath    string
	freqURL         string
	systemTable     string
	configPass      string
	stationIDs      map[int]bool
	extraHFDLArgs   []string
	dryRun          bool
	webPort         int
	webStaticDir    string
	iqRecordDir     string
	iqRecordSeconds int
}

func main() {
	var (
		ubersdrURL      = flag.String("url", "http://ubersdr:8080", "UberSDR base URL")
		password        = flag.String("pass", "", "Bypass password")
		ubersdrPath     = flag.String("ubersdr-iq", "ubersdr_iq", "Path to ubersdr_iq binary")
		dumphfdlPath    = flag.String("dumphfdl", "dumphfdl", "Path to dumphfdl binary")
		freqURL         = flag.String("freq-url", "https://ubersdr.org/hfdl/hfdl_frequencies.jsonl", "HFDL frequency list URL")
		stationFlag     = flag.String("station", "", "Comma-separated ground station IDs (default: all)")
		systemTable     = flag.String("system-table", "", "Path to dumphfdl system table file")
		configPass      = flag.String("config-pass", "", "Password to protect the Apply frequency endpoints")
		dryRun          = flag.Bool("dry-run", false, "Print planned instances without launching")
		webPort         = flag.Int("web-port", 6090, "Port for the web statistics server (0 = disabled)")
		webStatic       = flag.String("web-static", "/usr/local/share/hfdl_launcher/static", "Path to static web files directory")
		iqRecordDir     = flag.String("iq-record-dir", "", "Directory to write IQ WAV recordings (enables recording when set)")
		iqRecordSeconds = flag.Int("iq-record-seconds", 30, "Duration of each IQ recording in seconds")
	)
	flag.Usage = func() { printUsage(os.Stderr) }
	flag.Parse()

	stationIDs := make(map[int]bool)
	if *stationFlag != "" {
		for _, part := range strings.Split(*stationFlag, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.Atoi(part)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: invalid station ID %q in -station flag\n", part) //nolint:errcheck
				os.Exit(1)
			}
			stationIDs[id] = true
		}
	}

	if err := run(config{
		ubersdrURL:      *ubersdrURL,
		password:        *password,
		ubersdrPath:     *ubersdrPath,
		dumphfdlPath:    *dumphfdlPath,
		freqURL:         *freqURL,
		systemTable:     *systemTable,
		configPass:      *configPass,
		stationIDs:      stationIDs,
		extraHFDLArgs:   flag.Args(),
		dryRun:          *dryRun,
		webPort:         *webPort,
		webStaticDir:    *webStatic,
		iqRecordDir:     *iqRecordDir,
		iqRecordSeconds: *iqRecordSeconds,
	}); err != nil {
		log.Fatalf("error: %v", err)
	}
}
