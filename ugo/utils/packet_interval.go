package utils

// PacketInterval is an interval from one PacketNumber to the other
// +gen linkedlist
type PacketInterval struct {
	Start uint32
	End   uint32
}
