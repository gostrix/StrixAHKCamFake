package main

import (
	"os"
	"time"
)

// Config holds all application settings loaded from environment variables.
type Config struct {
	VideoFile string // path to local MP4 file
	VideoURL  string // URL to download MP4 from if VideoFile doesn't exist

	CameraName     string
	CameraModel    string
	CameraSerial   string
	CameraFirmware string

	Pin      string // HomeKit pairing PIN (8 digits, no dashes)
	HAPPort  string // TCP port for HAP (pair-setup, pair-verify, accessories)
	SRTPPort string // UDP port for SRTP video/audio streaming

	SnapshotInterval time.Duration
}

func LoadConfig() *Config {
	c := &Config{
		VideoFile:      env("VIDEO_FILE", "main.mp4"),
		VideoURL:       env("VIDEO_URL", "https://github.com/gostrix/StrixCamFake/raw/main/main.mp4"),
		CameraName:     env("CAMERA_NAME", "StrixCam"),
		CameraModel:    env("CAMERA_MODEL", "SAHK-1000"),
		CameraSerial:   env("CAMERA_SERIAL", "SAHK-001"),
		CameraFirmware: env("CAMERA_FIRMWARE", "1.0.0"),
		Pin:            env("PIN", "19550224"),
		HAPPort:        env("HAP_PORT", "51826"),
		SRTPPort:       env("SRTP_PORT", "8443"),

		SnapshotInterval: 5 * time.Second,
	}

	if s := os.Getenv("SNAPSHOT_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			c.SnapshotInterval = d
		}
	}

	return c
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
