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
//	  -url          UberSDR base URL (default: http://172.20.0.1:8080)
//	  -pass         Bypass password (optional)
//	  -ubersdr-iq   Path to ubersdr_iq binary (default: ubersdr_iq)
//	  -dumphfdl     Path to dumphfdl binary (default: dumphfdl)
//	  -freq-url     URL for HFDL frequency JSONL
//	  -station      Comma-separated ground station IDs to monitor (default: all)
//	  -system-table Path to dumphfdl system table file (optional)
//	  -dry-run      Print planned instances without launching
//	  -web-port     Port for the web statistics server (default: 8080, 0 = disabled)
//	  -web-static   Path to static web files directory

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
	fmt.Fprintf(w, "Usage: hfdl_launcher [flags] [-- dumphfdl-args...]\n\n")
	fmt.Fprintf(w, "Flags:\n")
	fmt.Fprintf(w, "  -url          UberSDR base URL (default: http://172.20.0.1:8080)\n")
	fmt.Fprintf(w, "  -pass         Bypass password (optional)\n")
	fmt.Fprintf(w, "  -ubersdr-iq   Path to ubersdr_iq binary (default: ubersdr_iq)\n")
	fmt.Fprintf(w, "  -dumphfdl     Path to dumphfdl binary (default: dumphfdl)\n")
	fmt.Fprintf(w, "  -freq-url     URL for HFDL frequency JSONL\n")
	fmt.Fprintf(w, "                (default: https://ubersdr.org/hfdl/hfdl_frequencies.jsonl)\n")
	fmt.Fprintf(w, "  -station      Comma-separated ground station IDs to monitor (default: all)\n")
	fmt.Fprintf(w, "  -system-table Path to dumphfdl system table file (optional)\n")
	fmt.Fprintf(w, "  -dry-run      Print planned instances without launching\n")
	fmt.Fprintf(w, "  -web-port     Port for the web statistics server (default: 6090, 0 = disabled)\n")
	fmt.Fprintf(w, "  -web-static   Path to static web files directory\n")
	fmt.Fprintf(w, "                (default: /usr/local/share/hfdl_launcher/static)\n\n")
	fmt.Fprintf(w, "Extra dumphfdl arguments:\n")
	fmt.Fprintf(w, "  Any arguments after -- are passed verbatim to every dumphfdl instance.\n")
	fmt.Fprintf(w, "  Note: --output decoded:json:file:path=- is always injected automatically.\n\n")
	fmt.Fprintf(w, "Bandwidth selection:\n")
	fmt.Fprintf(w, "  iq48  — 48 kHz  (channels clustered within 48 kHz)\n")
	fmt.Fprintf(w, "  iq96  — 96 kHz  (channels spanning 48–96 kHz)\n")
	fmt.Fprintf(w, "  iq192 — 192 kHz (channels spanning 96–192 kHz)\n")
	fmt.Fprintf(w, "  Clusters wider than 192 kHz are split across multiple windows.\n\n")
	fmt.Fprintf(w, "Examples:\n")
	fmt.Fprintf(w, "  hfdl_launcher -url http://sdr.example.com:8080\n")
	fmt.Fprintf(w, "  hfdl_launcher -url http://sdr.example.com:8080 -station 1,2,3\n")
	fmt.Fprintf(w, "  hfdl_launcher -url http://sdr.example.com:8080 -- --output decoded:json:tcp:address=host,port=5555\n")
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

	// Web server.
	if cfg.webPort > 0 {
		go startWebServer(cfg.webPort, cfg.webStaticDir, store, groups, fetched.DisabledFreqs, cfg.extraHFDLArgs, cfg.freqURL)
	}

	// Build and start instances.
	instances := make([]*instance, len(groups))
	for i, g := range groups {
		instances[i] = &instance{
			group:         g,
			ubersdrPath:   cfg.ubersdrPath,
			dumphfdlPath:  cfg.dumphfdlPath,
			ubersdrURL:    cfg.ubersdrURL,
			password:      cfg.password,
			systemTable:   cfg.systemTable,
			extraHFDLArgs: cfg.extraHFDLArgs,
			jsonCh:        jsonCh,
		}
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

	<-sigs
	log.Printf("shutting down all instances…")
	for _, inst := range instances {
		inst.stop()
	}
	time.Sleep(2 * time.Second)
	return nil
}

// config holds all launcher configuration.
type config struct {
	ubersdrURL    string
	password      string
	ubersdrPath   string
	dumphfdlPath  string
	freqURL       string
	systemTable   string
	stationIDs    map[int]bool
	extraHFDLArgs []string
	dryRun        bool
	webPort       int
	webStaticDir  string
}

func main() {
	var (
		ubersdrURL   = flag.String("url", "http://172.20.0.1:8080", "UberSDR base URL")
		password     = flag.String("pass", "", "Bypass password")
		ubersdrPath  = flag.String("ubersdr-iq", "ubersdr_iq", "Path to ubersdr_iq binary")
		dumphfdlPath = flag.String("dumphfdl", "dumphfdl", "Path to dumphfdl binary")
		freqURL      = flag.String("freq-url", "https://ubersdr.org/hfdl/hfdl_frequencies.jsonl", "HFDL frequency list URL")
		stationFlag  = flag.String("station", "", "Comma-separated ground station IDs (default: all)")
		systemTable  = flag.String("system-table", "", "Path to dumphfdl system table file")
		dryRun       = flag.Bool("dry-run", false, "Print planned instances without launching")
		webPort      = flag.Int("web-port", 6090, "Port for the web statistics server (0 = disabled)")
		webStatic    = flag.String("web-static", "/usr/local/share/hfdl_launcher/static", "Path to static web files directory")
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
				fmt.Fprintf(os.Stderr, "error: invalid station ID %q in -station flag\n", part)
				os.Exit(1)
			}
			stationIDs[id] = true
		}
	}

	if err := run(config{
		ubersdrURL:    *ubersdrURL,
		password:      *password,
		ubersdrPath:   *ubersdrPath,
		dumphfdlPath:  *dumphfdlPath,
		freqURL:       *freqURL,
		systemTable:   *systemTable,
		stationIDs:    stationIDs,
		extraHFDLArgs: flag.Args(),
		dryRun:        *dryRun,
		webPort:       *webPort,
		webStaticDir:  *webStatic,
	}); err != nil {
		log.Fatalf("error: %v", err)
	}
}
