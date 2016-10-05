package ugo

import (
	"math"
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

// InitialIdleConnectionStateLifetime is the initial idle connection state lifetime
// when we use ugo in http proxy, since http with keepalive option will
// keep connection open for a while, this value should be greater than that
const initialIdleConnectionStateLifetime = 300 * time.Second

// DefaultMaxCongestionWindow is the default for the max congestion window
// Taken from Chrome
const defaultMaxCongestionWindow uint64 = 200

// InitialCongestionWindow is the initial congestion window in QUIC packets
const initialCongestionWindow uint64 = 32

// maxByteCount is the maximum value of a uint32
const maxByteCount = math.MaxUint64

// InitialConnectionFlowControlWindow is the initial connection-level flow control window for sending
const InitialConnectionFlowControlWindow uint32 = (1 << 14) // 16 kB

// ackSendDelay is the maximal time delay applied to packets containing only ACKs
const ackSendDelay = 5 * time.Millisecond

// ReceiveConnectionFlowControlWindow is the stream-level flow control window for receiving data
// This is the value that Google servers are using
const ReceiveConnectionFlowControlWindow uint32 = (1 << 20) * 1.5 // 1.5 MB

// retransmissionThreshold + 1 is the number of times a packet has to be NACKed so that it gets retransmitted
const retransmissionThreshold uint8 = 3

// maxTrackedSentPackets is maximum number of sent packets saved for either later retransmission or entropy calculation
// TODO: find a reasonable value here
const maxTrackedSentPackets int = 2000

// MaxStreamFrameSorterGaps is the maximum number of gaps between received StreamFrames
// prevents DOS attacks against the streamFrameSorter
const MaxStreamFrameSorterGaps = 50
