package protocol

import "time"

// MaxCongestionWindow is the maximum size of the CWND, in packets.
// TODO: Unused?
const MaxCongestionWindow PacketNumber = 200

// DefaultMaxCongestionWindow is the default for the max congestion window
// Taken from Chrome
const DefaultMaxCongestionWindow PacketNumber = 107

// InitialCongestionWindow is the initial congestion window in QUIC packets
const InitialCongestionWindow PacketNumber = 32

// MaxUndecryptablePackets limits the number of undecryptable packets that a
// session queues for later until it sends a public reset.
const MaxUndecryptablePackets = 10

// AckSendDelay is the maximal time delay applied to packets containing only ACKs
const AckSendDelay = 5 * time.Millisecond

// ReceiveStreamFlowControlWindow is the stream-level flow control window for receiving data
// This is the value that Google servers are using
const ReceiveStreamFlowControlWindow uint32 = (1 << 20) // 1 MB

// ReceiveConnectionFlowControlWindow is the stream-level flow control window for receiving data
// This is the value that Google servers are using
const ReceiveConnectionFlowControlWindow uint32 = (1 << 20) * 1.5 // 1.5 MB

// MaxStreamsPerConnection is the maximum value accepted for the number of streams per connection
const MaxStreamsPerConnection uint32 = 100

// MaxStreamsMultiplier is the slack the client is allowed for the maximum number of streams per connection, needed e.g. when packets are out of order or dropped.
const MaxStreamsMultiplier = 1.1

// MaxIdleConnectionStateLifetime is the maximum value accepted for the idle connection state lifetime
// TODO: set a reasonable value here
const MaxIdleConnectionStateLifetime = 60 * time.Second

// MaxSessionUnprocessedPackets is the max number of packets stored in each session that are not yet processed.
const MaxSessionUnprocessedPackets = 128

// RetransmissionThreshold + 1 is the number of times a packet has to be NACKed so that it gets retransmitted
const RetransmissionThreshold uint8 = 3

// MaxTrackedSentPackets is maximum number of sent packets saved for either later retransmission or entropy calculation
// TODO: find a reasonable value here
// TODO: decrease this value after dropping support for QUIC 33 and earlier
const MaxTrackedSentPackets uint32 = 2000

// MaxTrackedReceivedPackets is the maximum number of received packets saved for doing the entropy calculations
// TODO: think about what to do with this when adding support for QUIC 34
const MaxTrackedReceivedPackets uint32 = 2000

// MaxStreamFrameSorterGaps is the maximum number of gaps between received StreamFrames
// prevents DOS attacks against the streamFrameSorter
const MaxStreamFrameSorterGaps = 50
