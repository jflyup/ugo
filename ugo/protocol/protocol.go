package protocol

import (
	"math"
	"time"
)

// A PacketNumber in QUIC
type PacketNumber uint32

// MaxByteCount is the maximum value of a uint32
const MaxByteCount = math.MaxUint32

// DefaultTCPMSS is the default maximum packet size used in the Linux TCP implementation.
// Used in QUIC for congestion window computations in bytes.
const DefaultTCPMSS uint32 = 1460

// InitialStreamFlowControlWindow is the initial stream-level flow control window for sending
const InitialStreamFlowControlWindow uint32 = (1 << 14) // 16 kB

// InitialConnectionFlowControlWindow is the initial connection-level flow control window for sending
const InitialConnectionFlowControlWindow uint32 = (1 << 14) // 16 kB

// InitialIdleConnectionStateLifetime is the initial idle connection state lifetime
const InitialIdleConnectionStateLifetime = 30 * time.Second
