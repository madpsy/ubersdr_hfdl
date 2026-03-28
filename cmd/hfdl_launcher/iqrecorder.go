package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// wavHeader writes a PCM WAV header for CS16 IQ data.
//
// CS16 is signed 16-bit interleaved I/Q — two channels (I and Q), 16 bits per
// sample, little-endian.  This maps directly to a standard 2-channel PCM WAV
// file at the given sample rate.
//
// dataBytes is the number of raw PCM bytes that will follow the header.
// Pass 0 to write a streaming/unknown-length header (data chunk size = 0xFFFFFFFF).
func writeWAVHeader(w io.Writer, sampleRateHz int, dataBytes uint32) error {
	const (
		numChannels   = 2  // I and Q
		bitsPerSample = 16 // CS16
		audioFormat   = 1  // PCM
	)
	byteRate := uint32(sampleRateHz * numChannels * bitsPerSample / 8)
	blockAlign := uint16(numChannels * bitsPerSample / 8)

	// RIFF chunk size = 36 + dataBytes (or 0xFFFFFFFF for streaming)
	var riffSize uint32
	if dataBytes == 0 {
		riffSize = 0xFFFFFFFF
	} else {
		riffSize = 36 + dataBytes
	}

	var dataCkSize uint32
	if dataBytes == 0 {
		dataCkSize = 0xFFFFFFFF
	} else {
		dataCkSize = dataBytes
	}

	buf := make([]byte, 44)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], riffSize)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(buf[20:22], audioFormat)
	binary.LittleEndian.PutUint16(buf[22:24], numChannels)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRateHz))
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataCkSize)

	_, err := w.Write(buf)
	return err
}

// recordIQ tees bytes from src into a WAV file under dir for the given
// duration, then continues copying the remainder of src into dst unmodified.
//
// The WAV file is named:
//
//	<dir>/iq_<centerKHz>kHz_<iqMode>_<timestamp>.wav
//
// Recording runs in the foreground for `duration`; after that the function
// returns and the caller can use dst normally.  If the file cannot be created
// the error is logged and the data is still forwarded to dst uninterrupted.
func recordIQ(src io.Reader, dst io.Writer, dir string, centerKHz int, iqMode string, sampleRateHz int, duration time.Duration) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	fname := fmt.Sprintf("iq_%dkHz_%s_%s.wav", centerKHz, iqMode, ts)
	fpath := filepath.Join(dir, fname)

	f, err := os.Create(fpath)
	if err != nil {
		log.Printf("[%d kHz / %s] IQ recorder: cannot create %s: %v — skipping recording", centerKHz, iqMode, fpath, err)
		// Still drain src → dst so the pipeline is not blocked.
		io.Copy(dst, src) //nolint:errcheck
		return
	}

	// Write a streaming WAV header (data size unknown up front).
	if err := writeWAVHeader(f, sampleRateHz, 0); err != nil {
		log.Printf("[%d kHz / %s] IQ recorder: write WAV header: %v — skipping recording", centerKHz, iqMode, err)
		f.Close()
		os.Remove(fpath)
		io.Copy(dst, src) //nolint:errcheck
		return
	}

	log.Printf("[%d kHz / %s] IQ recorder: recording %s for %s", centerKHz, iqMode, fpath, duration)

	// Tee into the WAV file for `duration`, then stop recording and pass the
	// rest of the stream straight through to dst.
	deadline := time.Now().Add(duration)
	buf := make([]byte, 32*1024)
	var written int64

	for time.Now().Before(deadline) {
		n, readErr := src.Read(buf)
		if n > 0 {
			// Write to WAV file
			if _, werr := f.Write(buf[:n]); werr != nil {
				log.Printf("[%d kHz / %s] IQ recorder: write error: %v — stopping recording early", centerKHz, iqMode, werr)
				break
			}
			written += int64(n)
			// Forward to dumphfdl stdin
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if readErr != nil {
			break
		}
	}

	// Patch the WAV header with the actual data size now that we know it.
	if written > 0 {
		patchWAVDataSize(f, uint32(written))
	}

	f.Close()
	log.Printf("[%d kHz / %s] IQ recorder: finished — wrote %d bytes to %s", centerKHz, iqMode, written, fpath)

	// Recording done — pass the rest of the stream through to dst.
	io.Copy(dst, src) //nolint:errcheck
}

// patchWAVDataSize seeks back into an open WAV file and overwrites the RIFF
// chunk size (bytes 4–7) and data chunk size (bytes 40–43) with the correct
// values now that the total number of data bytes is known.
func patchWAVDataSize(f *os.File, dataBytes uint32) {
	riffSize := 36 + dataBytes

	var b [4]byte

	binary.LittleEndian.PutUint32(b[:], riffSize)
	if _, err := f.WriteAt(b[:], 4); err != nil {
		log.Printf("IQ recorder: patch RIFF size: %v", err)
	}

	binary.LittleEndian.PutUint32(b[:], dataBytes)
	if _, err := f.WriteAt(b[:], 40); err != nil {
		log.Printf("IQ recorder: patch data size: %v", err)
	}
}
