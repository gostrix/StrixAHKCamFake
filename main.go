package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	loadDotEnv(".env")

	log.Logger = zerolog.New(zerolog.ConsoleWriter{
		Out: os.Stdout, TimeFormat: "15:04:05",
	}).With().Timestamp().Logger()

	cfg := LoadConfig()

	// ensure video file exists (download if needed)
	if err := ensureVideoFile(cfg.VideoFile, cfg.VideoURL); err != nil {
		log.Fatal().Err(err).Msg("video file not available")
	}

	absVideo, _ := filepath.Abs(cfg.VideoFile)
	log.Info().
		Str("video", absVideo).
		Str("pin", cfg.Pin).
		Str("name", cfg.CameraName).
		Msg("[app] starting StrixAHKCamFake")

	// create the video stream -- ffmpeg loops MP4 into an internal RTSP server,
	// then we pull H264/Opus RTP packets from it
	stream := NewStream()

	// start internal RTSP mini-server for ffmpeg to push into
	internalPort := startInternalRTSP(stream)

	// start ffmpeg to loop the MP4 file into the internal RTSP server
	time.Sleep(200 * time.Millisecond)
	startFFmpeg(absVideo, internalPort)

	// wait for the producer to connect
	waitForProducer(stream, 10*time.Second)

	// start snapshot loop
	snap := NewSnapshot()
	startSnapshotLoop(snap, internalPort, cfg.SnapshotInterval)

	// start the HomeKit camera server (mDNS + HAP + SRTP)
	startHomeKit(cfg, stream, snap)

	printInfo(cfg)

	select {}
}

func printInfo(cfg *Config) {
	fmt.Println()
	fmt.Println("=== StrixAHKCamFake ready ===")
	fmt.Println()
	fmt.Printf("  HomeKit camera:  %s\n", cfg.CameraName)
	fmt.Printf("  PIN:             %s\n", formatPin(cfg.Pin))
	fmt.Printf("  HAP port:        %s\n", cfg.HAPPort)
	fmt.Printf("  SRTP port:       %s\n", cfg.SRTPPort)
	fmt.Println()
	fmt.Println("  Open Apple Home app -> Add Accessory -> enter PIN above")
	fmt.Println()
}

// formatPin formats "19550224" -> "195-50-224"
func formatPin(pin string) string {
	if len(pin) == 8 {
		return pin[:3] + "-" + pin[3:5] + "-" + pin[5:]
	}
	return pin
}

func waitForProducer(stream *Stream, timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		if stream.HasProducer() {
			log.Info().Msg("[app] video producer connected")
			return
		}
		select {
		case <-deadline:
			log.Warn().Msg("[app] timeout waiting for video producer, continuing anyway")
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ensure http is imported (used by homekit server)
var _ http.Handler

// loadDotEnv reads a .env file and sets environment variables.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range splitLines(string(data)) {
		line = trimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		key, val, ok := cut(line, "=")
		if !ok {
			continue
		}
		key = trimSpace(key)
		val = trimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func splitLines(s string) []string {
	var lines []string
	for {
		i := indexOf(s, '\n')
		if i < 0 {
			if s != "" {
				lines = append(lines, s)
			}
			return lines
		}
		line := s[:i]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
		s = s[i+1:]
	}
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func cut(s, sep string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}
