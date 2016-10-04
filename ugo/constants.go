package ugo

import (
	"time"
)

const (
	packetInit = 0
)

const (
	aesEncrypt = iota
)

const (
	fecHeaderSize = 6
	typeData      = 0xf1
	typeFEC       = 0xf2
	fecExpire     = 30000 // 30s
)

// IPv4 minimum reassembly buffer size is 576, IPv6 has it at 1500.
// The maximum packet size, based on ethernet's max size,
// minus the IP and UDP headers. typical IPv4 has a 20 byte header, UDP adds an
// additional 4 bytes.  This is a total overhead of 24 bytes. Ethernet's
// max packet size is 1500 bytes,  1500 - 24 = 1476.
// TODO MTU discovery
const maxPacketSize uint64 = 1476

const minRetransmissionTime = 200 * time.Millisecond
const maxRetransmissionTime = 60 * time.Second
const defaultRetransmissionTime = 500 * time.Millisecond
const maxCongestionWindow uint32 = 200

// kind of net.ipv4.tcp_retries2,
// RFC 1122 recommends at least 100 seconds for the timeout,
// which corresponds to a value of at least 8.
// and linux has a TCP_USER_TIMEOUT socket option
const maxRetriesAttempted = 10

// kind of net.ipv4.tcp_syn_retries
const maxInitRetriesAttempted = 5

// DefaultMaxCongestionWindow is the default for the max congestion window
// Taken from Chrome
const DefaultMaxCongestionWindow uint64 = 107

// InitialCongestionWindow is the initial congestion window in QUIC packets
const InitialCongestionWindow uint64 = 32

// InitialIdleConnectionStateLifetime is the initial idle connection state lifetime
const InitialIdleConnectionStateLifetime = 60 * time.Second
