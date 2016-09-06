package utils

// ByteInterval is an interval from one ByteCount to the other
// +gen linkedlist
type ByteInterval struct {
	Start uint32
	End   uint32
}
