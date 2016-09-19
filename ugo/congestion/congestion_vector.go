package congestion

// PacketInfo combines packet number and length of a packet for congestion calculation
type PacketInfo struct {
	Number uint64
	Length uint32
}

// PacketVector is passed to the congestion algorithm
type PacketVector []PacketInfo
