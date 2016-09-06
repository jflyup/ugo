package protocol

func closestTo(target, a, b PacketNumber) PacketNumber {
	if delta(target, a) < delta(target, b) {
		return a
	}
	return b
}

func delta(a, b PacketNumber) PacketNumber {
	if a < b {
		return b - a
	}
	return a - b
}

// GetPacketNumberLength gets the minimum length needed to fully represent the packet number
func GetPacketNumberLength(packetNumber PacketNumber) PacketNumberLen {
	if packetNumber < (1 << (uint8(PacketNumberLen1) * 8)) {
		return PacketNumberLen1
	}
	if packetNumber < (1 << (uint8(PacketNumberLen2) * 8)) {
		return PacketNumberLen2
	}
	//	if packetNumber < (1 << (uint8(PacketNumberLen4) * 8)) {
	//		return PacketNumberLen4
	//	}
	return PacketNumberLen4
}
