package ugo

import (
	"errors"
	"log"
	"time"
)

var (
	errTimeLost = errors.New("packetReceiver: packet time lost")
)

var (
	errInvalidPacketNumber               = errors.New("packetReceiver: Invalid packet number")
	errTooManyOutstandingReceivedPackets = errors.New("TooManyOutstandingReceivedPackets")
)

type packetReceiver struct {
	largestInOrderObserved uint64
	largestObserved        uint64
	ignorePacketsBelow     uint64
	currentAckFrame        *sack
	// has an ACK for this state already been sent?
	// Will be set to false every time a new packet arrives,
	// and to false every time an ACK is sent
	stateChanged bool

	packetHistory *recvHistory

	receivedTimes         map[uint64]time.Time
	lowestInReceivedTimes uint64
}

// newPacketReceiver creates a new packetReceiver
func newPacketReceiver() *packetReceiver {
	return &packetReceiver{
		receivedTimes: make(map[uint64]time.Time),
		packetHistory: newRecvHistory(),
	}
}

func (h *packetReceiver) ReceivedPacket(packetNumber uint64) error {
	if packetNumber == 0 {
		return errInvalidPacketNumber
	}

	// if the packet number < StopWaiting,
	// we cannot detect if this packet has a duplicate number
	// the packet has to be ignored anyway
	if packetNumber <= h.ignorePacketsBelow {
		log.Printf("packet %d less than last StopWaiting %d", packetNumber, h.ignorePacketsBelow)
		return nil
	}

	_, ok := h.receivedTimes[packetNumber]
	if packetNumber <= h.largestInOrderObserved || ok {
		log.Printf("duplicate packet %d", packetNumber)
		return nil
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

func (h *packetReceiver) ReceivedStopWaiting(packetNumber uint64) error {
	// ignore if StopWaiting is unneeded, because we already received a StopWaiting with a higher LeastUnacked
	if h.ignorePacketsBelow >= packetNumber {
		return nil
	}

	h.ignorePacketsBelow = packetNumber - 1
	h.stateChanged = true
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

	log.Printf("largest in order observed:%d after receiving stopWait %d", h.largestInOrderObserved, packetNumber)

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
		largestAcked:       h.largestObserved,
		largestInOrder:     uint64(ackRanges[len(ackRanges)-1].firstPacketNumber),
		packetReceivedTime: packetReceivedTime,
	}

	if len(ackRanges) > 1 {
		h.currentAckFrame.ackRanges = ackRanges
	}

	return h.currentAckFrame, nil
}

func (h *packetReceiver) garbageCollectReceivedTimes() {
	for i := h.lowestInReceivedTimes; i <= h.ignorePacketsBelow; i++ {
		delete(h.receivedTimes, i)
	}
	h.lowestInReceivedTimes = h.ignorePacketsBelow
}
