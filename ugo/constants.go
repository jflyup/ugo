package ugo

import (
	"time"
)

const (
	PacketInit = 0
)

type EncryptType byte

const (
	AESEncrypt = EncryptType(iota)
)

const (
	fecHeaderSize      = 6
	fecHeaderSizePlus2 = fecHeaderSize + 2 // plus 2B data size
	typeData           = 0xf1
	typeFEC            = 0xf2
	fecExpire          = 30000 // 30s
)

// IPv4 minimum reassembly buffer size is 576, IPv6 has it at 1500.
// Subtract header sizes from here

const MinRetransmissionTime = 200 * time.Millisecond
const DefaultRetransmissionTime = 500 * time.Millisecond
const MaxCongestionWindow uint32 = 200

// DefaultMaxCongestionWindow is the default for the max congestion window
// Taken from Chrome
const DefaultMaxCongestionWindow uint32 = 107

// InitialCongestionWindow is the initial congestion window in QUIC packets
const InitialCongestionWindow uint32 = 32

// InitialIdleConnectionStateLifetime is the initial idle connection state lifetime
const InitialIdleConnectionStateLifetime = 30 * time.Second
