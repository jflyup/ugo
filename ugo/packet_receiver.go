package ugo

import (
	"errors"
	//"log"
	"time"
)

var (
	// ErrDuplicatePacket occurres when a duplicate packet is received
	ErrDuplicatePacket = errors.New("packetReceiver: Duplicate Packet")
	errTimeLost        = errors.New("packetReceiver: packet time lost")
	// ErrPacketSmallerThanLastStopWaiting occurs when a packet arrives with a packet number smaller than the largest LeastUnacked of a StopWaitingFrame. If this error occurs, the packet should be ignored
	ErrPacketSmallerThanLastStopWaiting = errors.New("packetReceiver: Packet number smaller than highest StopWaiting")
)

var (
	errInvalidPacketNumber               = errors.New("packetReceiver: Invalid packet number")
	errTooManyOutstandingReceivedPackets = errors.New("TooManyOutstandingReceivedPackets")
)

type packetReceiver struct {
	largestInOrderObserved uint32
	largestObserved        uint32
	ignorePacketsBelow     uint32
	currentAckFrame        *sack
	stateChanged           bool // has an ACK for this state already been sent? Will be set to false every time a new packet arrives, and to false every time an ACK is sent

	packetHistory *recvHistory

	receivedTimes         map[uint32]time.Time
	lowestInReceivedTimes uint32
}

// NewReceivedPacketHandler creates a new receivedPacketHandler
func newPacketReceiver() *packetReceiver {
	return &packetReceiver{
		receivedTimes: make(map[uint32]time.Time),
		packetHistory: newRecvHistory(),
	}
}

func (h *packetReceiver) ReceivedPacket(packetNumber uint32) error {
	if packetNumber == 0 {
		return errInvalidPacketNumber
	}

	// if the packet number < StopWaiting,
	// we cannot detect if this packet has a duplicate number
	// the packet has to be ignored anyway
	if packetNumber <= h.ignorePacketsBelow {
		return ErrPacketSmallerThanLastStopWaiting
	}

	_, ok := h.receivedTimes[packetNumber]
	if packetNumber <= h.largestInOrderObserved || ok {
		return ErrDuplicatePacket
	}

	h.packetHistory.ReceivedPacket(packetNumber)

	h.stateChanged = true
	h.currentAckFrame = nil

	if packetNumber > h.largestObserved {
		h.largestObserved = packetNumber
	}

	if packetNumber == h.largestInOrderObserved+1 {
		h.largestInOrderObserved = packetNumber
		h.packetHistory.DeleteBelow(h.largestInOrderObserved)
		delete(h.receivedTimes, packetNumber-1)
	}

	h.receivedTimes[packetNumber] = time.Now()
	h.lowestInReceivedTimes = h.largestInOrderObserved

	if uint32(len(h.receivedTimes)) > 1000 {
		return errTooManyOutstandingReceivedPackets
	}

	return nil
}

func (h *packetReceiver) ReceivedStopWaiting(packetNumber uint32) error {
	h.stateChanged = true
	// ignore if StopWaiting is unneeded, because we already received a StopWaiting with a higher LeastUnacked
	if h.ignorePacketsBelow >= packetNumber {
		return nil
	}

	h.ignorePacketsBelow = packetNumber - 1
	// h.garbageCollectReceivedTimes()

	// the LeastUnacked is the smallest packet number of any packet for
	// which the sender is still awaiting an ack.
	// So the largestInOrderObserved is one less than that
	if packetNumber > h.largestInOrderObserved {
		h.largestInOrderObserved = packetNumber - 1
	}

	// increase the largestInOrderObserved, if this is the lowest missing packet
	for i := h.largestInOrderObserved + 1; i <= h.largestObserved; i++ {
		_, ok := h.receivedTimes[i]
		if ok {
			h.largestInOrderObserved = i
			h.ignorePacketsBelow = i - 1
		} else {
			break
		}
	}

	h.packetHistory.DeleteBelow(h.largestInOrderObserved)
	h.garbageCollectReceivedTimes()

	// h.packetHistory.DeleteBelow(packetNumber)

	// ackRanges := h.packetHistory.GetAckRanges()
	// // TODO
	// if len(ackRanges) > 0 {
	// 	n := ackRanges[len(ackRanges)-1].LastPacketNumber
	// 	h.packetHistory.DeleteBelow(n)
	// 	h.largestInOrderObserved = n
	// 	h.ignorePacketsBelow = n
	// 	h.garbageCollectReceivedTimes()
	// }

	return nil
}

func (h *packetReceiver) GetAckFrame(dequeue bool) (*sack, error) {
	if !h.stateChanged {
		return nil, nil
	}

	if dequeue {
		h.stateChanged = false
	}

	if h.currentAckFrame != nil {
		return h.currentAckFrame, nil
	}

	packetReceivedTime, ok := h.receivedTimes[h.largestObserved]
	if !ok {
		return nil, errTimeLost
	}
	// packetReceivedTime := time.Now()
	ackRanges := h.packetHistory.GetAckRanges()
	h.currentAckFrame = &sack{
		LargestAcked:       h.largestObserved,
		LargestInOrder:     ackRanges[len(ackRanges)-1].FirstPacketNumber,
		PacketReceivedTime: packetReceivedTime,
	}

	if len(ackRanges) > 1 {
		h.currentAckFrame.AckRanges = ackRanges
	}

	return h.currentAckFrame, nil
}

func (h *packetReceiver) garbageCollectReceivedTimes() {
	for i := h.lowestInReceivedTimes; i <= h.ignorePacketsBelow; i++ {
		delete(h.receivedTimes, i)
	}
	h.lowestInReceivedTimes = h.ignorePacketsBelow
}
