package main

import (
	"errors"
	"io"
	"net"
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/rs/zerolog/log"
)

// startInternalRTSP starts a minimal RTSP server on a random port for ffmpeg to push into.
// Returns the port number.
func startInternalRTSP(stream *Stream) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal().Err(err).Msg("[rtsp] listen failed")
	}

	port := ln.Addr().(*net.TCPAddr).Port
	log.Debug().Int("port", port).Msg("[rtsp] internal server listening")

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Error().Err(err).Msg("[rtsp] accept")
				return
			}
			go handleInternalRTSP(conn, stream)
		}
	}()

	return port
}

func handleInternalRTSP(conn net.Conn, stream *Stream) {
	c := rtsp.NewServer(conn)

	c.Listen(func(msg any) {
		switch msg {
		case rtsp.MethodDescribe:
			// consumers would DESCRIBE, but we only use this for ffmpeg ANNOUNCE
			if c.URL == nil {
				return
			}

			c.SessionName = "StrixAHKCamFake"
			c.Medias = []*core.Media{
				{
					Kind:      core.KindVideo,
					Direction: core.DirectionSendonly,
					Codecs:    []*core.Codec{{Name: core.CodecH264}},
				},
				{
					Kind:      core.KindAudio,
					Direction: core.DirectionSendonly,
					Codecs:    []*core.Codec{{Name: core.CodecOpus}, {Name: core.CodecAAC}},
				},
			}

			if err := stream.AddConsumer(c); err != nil {
				log.Warn().Err(err).Msg("[rtsp] add consumer")
			}

		case rtsp.MethodAnnounce:
			// ffmpeg pushes stream via ANNOUNCE
			if c.URL == nil {
				return
			}
			name := strings.TrimPrefix(c.URL.Path, "/")
			log.Info().Str("stream", name).Msg("[rtsp] producer connected via ANNOUNCE")
			stream.SetProducer(c)
		}
	})

	if err := c.Accept(); err != nil {
		if !errors.Is(err, io.EOF) {
			log.Trace().Err(err).Msg("[rtsp] accept error")
		}
		_ = conn.Close()
		return
	}

	if err := c.Handle(); err != nil {
		log.Debug().Err(err).Msg("[rtsp] handle")
	}

	_ = conn.Close()
}
