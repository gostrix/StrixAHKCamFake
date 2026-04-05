package main

import (
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/rs/zerolog/log"
)

// Stream connects one Producer (ffmpeg via RTSP ANNOUNCE) to many Consumers (HomeKit SRTP sessions).
type Stream struct {
	mu        sync.Mutex
	prod      core.Producer
	receivers []*core.Receiver
	consumers []core.Consumer
}

func NewStream() *Stream {
	return &Stream{}
}

// SetProducer assigns a producer and extracts its media receivers.
func (s *Stream) SetProducer(prod core.Producer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.prod = prod
	s.receivers = nil

	medias := prod.GetMedias()
	log.Debug().Int("medias", len(medias)).Msg("[stream] set producer")

	for _, media := range medias {
		for _, codec := range media.Codecs {
			log.Debug().Str("codec", codec.Name).Uint32("clock", codec.ClockRate).Msg("[stream] producer codec")
			track, err := prod.GetTrack(media, codec)
			if err != nil {
				log.Debug().Err(err).Msg("[stream] get track failed")
				continue
			}
			s.receivers = append(s.receivers, track)
		}
	}

	log.Debug().Int("receivers", len(s.receivers)).Msg("[stream] receivers ready")
}

// AddConsumer connects a consumer to matching producer tracks.
func (s *Stream) AddConsumer(cons core.Consumer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	consMedias := cons.GetMedias()
	for _, consMedia := range consMedias {
		for _, receiver := range s.receivers {
			prodCodec := receiver.Codec
			if consMedia.Kind != core.GetKind(prodCodec.Name) {
				continue
			}
			var consCodec *core.Codec
			for _, cc := range consMedia.Codecs {
				if prodCodec.Match(cc) {
					consCodec = cc
					break
				}
			}
			if consCodec == nil {
				continue
			}
			log.Debug().Str("codec", consCodec.Name).Msg("[stream] matched, adding track")
			if err := cons.AddTrack(consMedia, consCodec, receiver); err != nil {
				log.Debug().Err(err).Msg("[stream] add track failed")
				continue
			}
			break
		}
	}

	s.consumers = append(s.consumers, cons)
	return nil
}

// RemoveConsumer disconnects a consumer.
func (s *Stream) RemoveConsumer(cons core.Consumer) {
	_ = cons.Stop()

	s.mu.Lock()
	for i, c := range s.consumers {
		if c == cons {
			s.consumers = append(s.consumers[:i], s.consumers[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

// HasProducer returns true if a producer is connected and has receivers.
func (s *Stream) HasProducer() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prod != nil && len(s.receivers) > 0
}
