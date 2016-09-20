package protocol

import (
	"math"
	"time"
)

// A PacketNumber in QUIC
type PacketNumber uint32

// A ConnectionID in QUIC
type ConnectionID uint32

// The maximum packet size, based on ethernet's max size,
// minus the IP and UDP headers. typical IPv4 has a 20 byte header, UDP adds an
// additional 4 bytes.  This is a total overhead of 24 bytes.  Ethernet's
// max packet size is 1500 bytes,  1500 - 24 = 1476.
const MaxPacketSize uint64 = 1476

// MaxByteCount is the maximum value of a uint32
const MaxByteCount = math.MaxUint32

// MaxFrameAndPublicHeaderSize is the maximum size of a QUIC frame plus PublicHeader
const MaxFrameAndPublicHeaderSize = MaxPacketSize - 12 /*crypto signature*/

// DefaultTCPMSS is the default maximum packet size used in the Linux TCP implementation.
// Used in QUIC for congestion window computations in bytes.
const DefaultTCPMSS uint32 = 1460

// InitialStreamFlowControlWindow is the initial stream-level flow control window for sending
const InitialStreamFlowControlWindow uint32 = (1 << 14) // 16 kB

// InitialConnectionFlowControlWindow is the initial connection-level flow control window for sending
const InitialConnectionFlowControlWindow uint32 = (1 << 14) // 16 kB

// InitialIdleConnectionStateLifetime is the initial idle connection state lifetime
const InitialIdleConnectionStateLifetime = 30 * time.Second

// DefaultRetransmissionTime is the RTO time on new connections
const DefaultRetransmissionTime = 500 * time.Millisecond

// MinRetransmissionTime is the minimum RTO time
const MinRetransmissionTime = 200 * time.Millisecond

// ClientHelloMinimumSize is the minimum size the server expectes an inchoate CHLO to have.
const ClientHelloMinimumSize = 1024
