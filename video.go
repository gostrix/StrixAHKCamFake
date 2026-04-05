package main

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/rs/zerolog/log"
)

// ensureVideoFile checks if the video file exists locally.
// If not, downloads it from the given URL.
func ensureVideoFile(path, url string) error {
	if _, err := os.Stat(path); err == nil {
		log.Info().Str("file", path).Msg("[video] using local file")
		return nil
	}

	if url == "" {
		return fmt.Errorf("video file %q not found and no download URL configured", path)
	}

	log.Info().Str("url", url).Str("dest", path).Msg("[video] downloading")

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("write file: %w", err)
	}

	log.Info().Int64("bytes", n).Msg("[video] download complete")
	return nil
}
