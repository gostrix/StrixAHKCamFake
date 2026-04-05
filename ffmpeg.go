package main

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/rs/zerolog/log"
)

// startFFmpeg launches ffmpeg to push a looping MP4 file into the internal RTSP server via ANNOUNCE.
// It restarts automatically on failure.
func startFFmpeg(videoPath string, rtspPort int) {
	url := fmt.Sprintf("rtsp://127.0.0.1:%d/live", rtspPort)

	go func() {
		for {
			cmd := exec.Command("ffmpeg",
				"-re", "-stream_loop", "-1",
				"-i", videoPath,
				"-c", "copy",
				"-rtsp_transport", "tcp",
				"-f", "rtsp", url,
			)

			log.Info().Str("file", videoPath).Int("port", rtspPort).Msg("[ffmpeg] starting")

			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Error().Err(err).Bytes("output", lastN(out, 512)).Msg("[ffmpeg] exited")
			}

			time.Sleep(3 * time.Second)
		}
	}()
}

func lastN(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
