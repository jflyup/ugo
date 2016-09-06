package congestion

import (
	"time"
)

// A SendAlgorithm performs congestion control and calculates the congestion window
type SendAlgorithm interface {
	TimeUntilSend(now time.Time, bytesInFlight uint32) time.Duration
	OnPacketSent(sentTime time.Time, bytesInFlight uint32, packetNumber uint32, bytes uint32, isRetransmittable bool) bool
	GetCongestionWindow() uint32
	OnCongestionEvent(rttUpdated bool, bytesInFlight uint32, ackedPackets PacketVector, lostPackets PacketVector)
	SetNumEmulatedConnections(n int)
	OnRetransmissionTimeout(packetsRetransmitted bool)
	OnConnectionMigration()
	RetransmissionDelay() time.Duration

	// Experiments
	SetSlowStartLargeReduction(enabled bool)
}

// SendAlgorithmWithDebugInfo adds some debug functions to SendAlgorithm
type SendAlgorithmWithDebugInfo interface {
	SendAlgorithm
	BandwidthEstimate() Bandwidth

	// Stuff only used in testing

	HybridSlowStart() *HybridSlowStart
	SlowstartThreshold() uint32
	RenoBeta() float32
	InRecovery() bool
}
