package main

import (
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/hap"
	"github.com/AlexxIT/go2rtc/pkg/hap/camera"
	"github.com/AlexxIT/go2rtc/pkg/hap/tlv8"
	"github.com/AlexxIT/go2rtc/pkg/homekit"
	"github.com/AlexxIT/go2rtc/pkg/mdns"
	"github.com/AlexxIT/go2rtc/pkg/opus"
	"github.com/AlexxIT/go2rtc/pkg/srtp"
	"github.com/pion/rtp"
	"github.com/rs/zerolog/log"
)

// startHomeKit registers the camera via mDNS and starts the HAP + SRTP servers.
func startHomeKit(cfg *Config, stream *Stream, snap *Snapshot) {
	pin, err := hap.SanitizePin(cfg.Pin)
	if err != nil {
		log.Fatal().Err(err).Msg("[homekit] invalid PIN")
	}

	deviceID := calcDeviceID(cfg.CameraName)
	setupID := calcSetupID(cfg.CameraName)
	devicePrivate := calcDevicePrivate(cfg.CameraName)
	name := cfg.CameraName

	// SRTP server for video/audio streaming
	srtpAddr := ":" + cfg.SRTPPort
	srtpServer := srtp.NewServer(srtpAddr)

	srv := &hkServer{
		stream:     stream,
		snap:       snap,
		srtpServer: srtpServer,
		setupID:    setupID,
		accessory:  camera.NewAccessory("Strix", cfg.CameraModel, name, cfg.CameraSerial, cfg.CameraFirmware),
	}

	srv.hap = &hap.Server{
		Pin:             pin,
		DeviceID:        deviceID,
		DevicePrivate:   devicePrivate,
		GetClientPublic: srv.GetPair,
	}

	// listen first, then use the real port for mDNS (supports port 0 = random)
	ln, err := net.Listen("tcp", ":"+cfg.HAPPort)
	if err != nil {
		log.Fatal().Err(err).Msg("[homekit] HAP listen failed")
	}
	hapPort := ln.Addr().(*net.TCPAddr).Port

	entry := &mdns.ServiceEntry{
		Name: name,
		Port: uint16(hapPort),
		Info: map[string]string{
			hap.TXTConfigNumber: "1",
			hap.TXTFeatureFlags: "0",
			hap.TXTDeviceID:     deviceID,
			hap.TXTModel:        cfg.CameraModel,
			hap.TXTProtoVersion: "1.1",
			hap.TXTStateNumber:  "1",
			hap.TXTStatusFlags:  hap.StatusNotPaired,
			hap.TXTCategory:     hap.CategoryCamera,
			hap.TXTSetupHash:    hap.SetupHash(setupID, deviceID),
		},
	}
	srv.mdns = entry
	srv.updateStatus()

	// start HAP HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc(hap.PathPairSetup, func(w http.ResponseWriter, r *http.Request) {
		srv.handleHAP(w, r)
	})
	mux.HandleFunc(hap.PathPairVerify, func(w http.ResponseWriter, r *http.Request) {
		srv.handleHAP(w, r)
	})

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("[homekit] HAP serve")
		}
	}()

	log.Info().Int("port", hapPort).Str("device_id", deviceID).Msg("[homekit] HAP server listening")

	// start mDNS advertisement
	go func() {
		if err := mdns.Serve(mdns.ServiceHAP, []*mdns.ServiceEntry{entry}); err != nil {
			log.Error().Err(err).Msg("[homekit] mDNS serve failed")
		}
	}()

	log.Info().Str("name", name).Msg("[homekit] mDNS advertising")
}

// hkServer holds all state for the HomeKit camera server.
type hkServer struct {
	hap  *hap.Server
	mdns *mdns.ServiceEntry

	stream     *Stream
	snap       *Snapshot
	srtpServer *srtp.Server

	pairings []string
	conns    []any
	mu       sync.Mutex

	accessory *hap.Accessory
	consumer  *hkConsumer
	setupID   string
}

func (s *hkServer) handleHAP(w http.ResponseWriter, r *http.Request) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	r.Body = io.NopCloser(rw)

	switch r.RequestURI {
	case hap.PathPairSetup:
		id, key, err := s.hap.PairSetup(r, rw)
		if err != nil {
			log.Error().Err(err).Msg("[homekit] pair-setup failed")
			return
		}
		s.addPair(id, key, hap.PermissionAdmin)

	case hap.PathPairVerify:
		id, key, err := s.hap.PairVerify(r, rw)
		if err != nil {
			log.Debug().Err(err).Msg("[homekit] pair-verify failed")
			return
		}

		log.Debug().Str("client_id", id).Str("remote", conn.RemoteAddr().String()).Msg("[homekit] new verified connection")

		controller, err := hap.NewConn(conn, rw, key, false)
		if err != nil {
			log.Error().Err(err).Msg("[homekit] new conn failed")
			return
		}

		s.addConn(controller)
		defer s.delConn(controller)

		handler := homekit.ServerHandler(s)

		if err = handler(controller); err != nil && !errors.Is(err, io.EOF) {
			log.Error().Err(err).Msg("[homekit] handler error")
		}
	}
}

// --- ServerPair interface ---

func (s *hkServer) GetPair(id string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	if i := s.pairIndex(id); i >= 0 {
		query, _ := url.ParseQuery(s.pairings[i])
		b, _ := hex.DecodeString(query.Get("client_public"))
		return b
	}
	return nil
}

func (s *hkServer) AddPair(id string, public []byte, permissions byte) {
	s.addPair(id, public, permissions)
}

func (s *hkServer) DelPair(id string) {
	s.mu.Lock()
	if i := s.pairIndex(id); i >= 0 {
		s.pairings = append(s.pairings[:i], s.pairings[i+1:]...)
		s.updateStatus()
	}
	s.mu.Unlock()

	log.Debug().Str("id", id).Msg("[homekit] del pair")
}

func (s *hkServer) addPair(id string, public []byte, permissions byte) {
	log.Debug().Str("id", id).Hex("public", public).Uint8("perm", permissions).Msg("[homekit] add pair")

	s.mu.Lock()
	if s.pairIndex(id) < 0 {
		s.pairings = append(s.pairings, fmt.Sprintf(
			"client_id=%s&client_public=%x&permissions=%d", id, public, permissions,
		))
		s.updateStatus()
	}
	s.mu.Unlock()
}

func (s *hkServer) pairIndex(id string) int {
	id = "client_id=" + id
	for i, pairing := range s.pairings {
		if strings.HasPrefix(pairing, id) {
			return i
		}
	}
	return -1
}

func (s *hkServer) updateStatus() {
	if len(s.pairings) == 0 {
		s.mdns.Info[hap.TXTStatusFlags] = hap.StatusNotPaired
	} else {
		s.mdns.Info[hap.TXTStatusFlags] = hap.StatusPaired
	}
}

// --- ServerAccessory interface ---

func (s *hkServer) GetAccessories(_ net.Conn) []*hap.Accessory {
	return []*hap.Accessory{s.accessory}
}

func (s *hkServer) GetCharacteristic(_ net.Conn, aid uint8, iid uint64) any {
	char := s.accessory.GetCharacterByID(iid)
	if char == nil {
		return nil
	}

	switch char.Type {
	case camera.TypeSetupEndpoints:
		consumer := s.consumer
		if consumer == nil {
			return nil
		}
		answer := consumer.GetAnswer()
		v, err := tlv8.MarshalBase64(answer)
		if err != nil {
			return nil
		}
		return v
	}

	return char.Value
}

func (s *hkServer) SetCharacteristic(conn net.Conn, aid uint8, iid uint64, value any) {
	char := s.accessory.GetCharacterByID(iid)
	if char == nil {
		return
	}

	switch char.Type {
	case camera.TypeSetupEndpoints:
		var offer camera.SetupEndpointsRequest
		if err := tlv8.UnmarshalBase64(value, &offer); err != nil {
			return
		}
		consumer := newHKConsumer(conn, s.srtpServer)
		consumer.SetOffer(&offer)
		s.consumer = consumer

	case camera.TypeSelectedStreamConfiguration:
		var conf camera.SelectedStreamConfiguration
		if err := tlv8.UnmarshalBase64(value, &conf); err != nil {
			return
		}

		log.Debug().Str("session", conf.Control.SessionID).Uint8("cmd", conf.Control.Command).Msg("[homekit] stream control")

		switch conf.Control.Command {
		case camera.SessionCommandEnd:
			for _, c := range s.conns {
				if consumer, ok := c.(*hkConsumer); ok {
					if consumer.sessionID == conf.Control.SessionID {
						_ = consumer.Stop()
						return
					}
				}
			}

		case camera.SessionCommandStart:
			consumer := s.consumer
			if consumer == nil {
				return
			}

			if !consumer.SetConfig(&conf) {
				log.Warn().Msg("[homekit] wrong config")
				return
			}

			s.addConn(consumer)

			if err := s.stream.AddConsumer(consumer); err != nil {
				log.Error().Err(err).Msg("[homekit] add consumer failed")
				return
			}

			go func() {
				_, _ = consumer.WriteTo(nil)
				s.stream.RemoveConsumer(consumer)
				s.delConn(consumer)
			}()
		}
	}
}

func (s *hkServer) GetImage(_ net.Conn, width, height int) []byte {
	data := s.snap.Get()
	if data == nil {
		return nil
	}
	// return the cached snapshot as-is (already JPEG)
	return data
}

// --- connection tracking ---

func (s *hkServer) addConn(v any) {
	s.mu.Lock()
	s.conns = append(s.conns, v)
	s.mu.Unlock()
}

func (s *hkServer) delConn(v any) {
	s.mu.Lock()
	if i := slices.Index(s.conns, v); i >= 0 {
		s.conns = slices.Delete(s.conns, i, i+1)
	}
	s.mu.Unlock()
}

// --- hkConsumer: sends H264+Opus via SRTP to the Apple device ---

type hkConsumer struct {
	core.Connection
	conn       net.Conn
	srtpServer *srtp.Server

	deadline *time.Timer

	sessionID    string
	videoSession *srtp.Session
	audioSession *srtp.Session
	audioRTPTime byte
}

func newHKConsumer(conn net.Conn, server *srtp.Server) *hkConsumer {
	medias := []*core.Media{
		{
			Kind:      core.KindVideo,
			Direction: core.DirectionSendonly,
			Codecs:    []*core.Codec{{Name: core.CodecH264}},
		},
		{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs:    []*core.Codec{{Name: core.CodecOpus}},
		},
	}
	return &hkConsumer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "homekit",
			Protocol:   "srtp",
			RemoteAddr: conn.RemoteAddr().String(),
			Medias:     medias,
			Transport:  conn,
		},
		conn:       conn,
		srtpServer: server,
	}
}

func (c *hkConsumer) SessionID() string {
	return c.sessionID
}

func (c *hkConsumer) SetOffer(offer *camera.SetupEndpointsRequest) {
	c.sessionID = offer.SessionID
	c.videoSession = &srtp.Session{
		Remote: &srtp.Endpoint{
			Addr:       offer.Address.IPAddr,
			Port:       offer.Address.VideoRTPPort,
			MasterKey:  []byte(offer.VideoCrypto.MasterKey),
			MasterSalt: []byte(offer.VideoCrypto.MasterSalt),
		},
	}
	c.audioSession = &srtp.Session{
		Remote: &srtp.Endpoint{
			Addr:       offer.Address.IPAddr,
			Port:       offer.Address.AudioRTPPort,
			MasterKey:  []byte(offer.AudioCrypto.MasterKey),
			MasterSalt: []byte(offer.AudioCrypto.MasterSalt),
		},
	}
}

func (c *hkConsumer) GetAnswer() *camera.SetupEndpointsResponse {
	c.videoSession.Local = c.srtpEndpoint()
	c.audioSession.Local = c.srtpEndpoint()

	return &camera.SetupEndpointsResponse{
		SessionID: c.sessionID,
		Status:    camera.StreamingStatusAvailable,
		Address: camera.Address{
			IPAddr:       c.videoSession.Local.Addr,
			VideoRTPPort: c.videoSession.Local.Port,
			AudioRTPPort: c.audioSession.Local.Port,
		},
		VideoCrypto: camera.SRTPCryptoSuite{
			MasterKey:  string(c.videoSession.Local.MasterKey),
			MasterSalt: string(c.videoSession.Local.MasterSalt),
		},
		AudioCrypto: camera.SRTPCryptoSuite{
			MasterKey:  string(c.audioSession.Local.MasterKey),
			MasterSalt: string(c.audioSession.Local.MasterSalt),
		},
		VideoSSRC: c.videoSession.Local.SSRC,
		AudioSSRC: c.audioSession.Local.SSRC,
	}
}

func (c *hkConsumer) SetConfig(conf *camera.SelectedStreamConfiguration) bool {
	if c.sessionID != conf.Control.SessionID {
		return false
	}

	c.SDP = fmt.Sprintf("%+v\n%+v", conf.VideoCodec, conf.AudioCodec)

	c.videoSession.Remote.SSRC = conf.VideoCodec.RTPParams[0].SSRC
	c.videoSession.PayloadType = conf.VideoCodec.RTPParams[0].PayloadType
	c.videoSession.RTCPInterval = toDuration(conf.VideoCodec.RTPParams[0].RTCPInterval)

	c.audioSession.Remote.SSRC = conf.AudioCodec.RTPParams[0].SSRC
	c.audioSession.PayloadType = conf.AudioCodec.RTPParams[0].PayloadType
	c.audioSession.RTCPInterval = toDuration(conf.AudioCodec.RTPParams[0].RTCPInterval)
	c.audioRTPTime = conf.AudioCodec.CodecParams[0].RTPTime[0]

	c.srtpServer.AddSession(c.videoSession)
	c.srtpServer.AddSession(c.audioSession)

	return true
}

func (c *hkConsumer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	var session *srtp.Session
	if codec.Kind() == core.KindVideo {
		session = c.videoSession
	} else {
		session = c.audioSession
	}

	sender := core.NewSender(media, track.Codec)

	if c.deadline == nil {
		c.deadline = time.NewTimer(time.Second * 30)

		sender.Handler = func(packet *rtp.Packet) {
			c.deadline.Reset(core.ConnDeadline)
			if n, err := session.WriteRTP(packet); err == nil {
				c.Send += n
			}
		}
	} else {
		sender.Handler = func(packet *rtp.Packet) {
			if n, err := session.WriteRTP(packet); err == nil {
				c.Send += n
			}
		}
	}

	switch codec.Name {
	case core.CodecH264:
		sender.Handler = h264.RTPPay(1378, sender.Handler)
		// RepairAVCC ensures SPS/PPS are prepended before every IDR keyframe.
		// Without this, decoders receiving the SRTP stream (or RTSP restream)
		// won't be able to initialize until they see SPS/PPS inline.
		sender.Handler = h264.RepairAVCC(track.Codec, sender.Handler)
		if track.Codec.IsRTP() {
			sender.Handler = h264.RTPDepay(track.Codec, sender.Handler)
		}
	case core.CodecOpus:
		sender.Handler = opus.RepackToHAP(c.audioRTPTime, sender.Handler)
	}

	sender.HandleRTP(track)
	c.Senders = append(c.Senders, sender)
	return nil
}

func (c *hkConsumer) WriteTo(io.Writer) (int64, error) {
	if c.deadline != nil {
		<-c.deadline.C
	}
	return 0, nil
}

func (c *hkConsumer) Stop() error {
	if c.deadline != nil {
		c.deadline.Reset(0)
	}
	return c.Connection.Stop()
}

func (c *hkConsumer) srtpEndpoint() *srtp.Endpoint {
	addr := c.conn.LocalAddr().(*net.TCPAddr)
	return &srtp.Endpoint{
		Addr:       addr.IP.To4().String(),
		Port:       uint16(c.srtpServer.Port()),
		MasterKey:  []byte(core.RandString(16, 0)),
		MasterSalt: []byte(core.RandString(14, 0)),
		SSRC:       rand.Uint32(),
	}
}

func toDuration(seconds float32) time.Duration {
	return time.Duration(seconds * float32(time.Second))
}

// --- helpers for deterministic crypto keys from seed ---

func calcDeviceID(seed string) string {
	b := sha512.Sum512([]byte(seed))
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[32], b[34], b[36], b[38], b[40], b[42])
}

func calcDevicePrivate(seed string) []byte {
	b := sha512.Sum512([]byte(seed))
	return ed25519.NewKeyFromSeed(b[:ed25519.SeedSize])
}

func calcSetupID(seed string) string {
	b := sha512.Sum512([]byte(seed))
	return fmt.Sprintf("%02X%02X", b[44], b[46])
}

// ensure json is imported
var _ = json.Marshal
