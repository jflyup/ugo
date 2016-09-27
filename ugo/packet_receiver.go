package ugo

import (
	"errors"
	"log"
	"time"
)

var (
	errTimeLost                          = errors.New("packetReceiver: packet time lost")
	errInvalidPacketNumber               = errors.New("packetReceiver: Invalid packet number")
	errTooManyOutstandingReceivedPackets = errors.New("packetReceiver: Too Many Outstanding Received Packets")
)

type packetReceiver struct {
	largestInOrderObserved uint64
	largestObserved        uint64
	ignorePacketsBelow     uint64
	currentSack            *sack
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

	// ignore packet below stopWaiting
	if packetNumber <= h.ignorePacketsBelow {
		log.Printf("packet %d < last StopWaiting %d", packetNumber, h.ignorePacketsBelow)
		return nil
	}

	_, ok := h.receivedTimes[packetNumber]
	if packetNumber <= h.largestInOrderObserved || ok {
		log.Printf("duplicate packet %d", packetNumber)
		return nil
	}

	h.packetHistory.ReceivedPacket(packetNumber)

	h.stateChanged = true
	h.currentSack = nil

	if packetNumber > h.largestObserved {
		h.largestObserved = packetNumber
	}

	if packetNumber == h.largestInOrderObserved+1 {
		h.largestInOrderObserved = packetNumber
		delete(h.receivedTimes, h.largestInOrderObserved-1)
		// try to increase the largestInOrderObserved if out-of-order packet arrives
		for i := h.largestInOrderObserved + 1; i <= h.largestObserved; i++ {
			_, ok := h.receivedTimes[i]
			if ok {
				h.largestInOrderObserved++
				delete(h.receivedTimes, h.largestInOrderObserved-1)
			}
		}

		h.packetHistory.DeleteBelow(h.largestInOrderObserved)

		// verify the correctness of ack
		if len(h.packetHistory.getAckRanges()) == 1 {
			// if no missing packet, largestInOrderObserved should be equal with largestObserved
			if h.largestInOrderObserved != h.largestObserved {
				log.Println("BUG: no missing packet, but largestInOrderObserved != largestObserved")
			}
		}
	}

	h.receivedTimes[packetNumber] = time.Now()
	h.lowestInReceivedTimes = h.largestInOrderObserved

	if len(h.receivedTimes) > 1000 {
		return errTooManyOutstandingReceivedPackets
	}

	return nil
}

func (h *packetReceiver) receivedStopWaiting(stopWaiting uint64) error {
	// ignore if StopWaiting is unneeded, because we already received a StopWaiting with a higher LeastUnacked
	if h.ignorePacketsBelow >= stopWaiting {
		return nil
	}

	h.ignorePacketsBelow = stopWaiting - 1
	h.stateChanged = true
	// the stopWaiting is the smallest packet number of any packet for
	// which the sender is still awaiting an ack.
	// So the largestInOrderObserved is one less than it
	if stopWaiting > h.largestInOrderObserved {
		h.largestInOrderObserved = stopWaiting - 1
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

	log.Printf("largest in order observed:%d after receiving stopWait %d", h.largestInOrderObserved, stopWaiting)

	return nil
}

func (h *packetReceiver) buildSack(dequeue bool) (*sack, error) {
	if !h.stateChanged {
		return nil, nil
	}

	if dequeue {
		h.stateChanged = false
	}

	if h.currentSack != nil {
		return h.currentSack, nil
	}

	packetReceivedTime, ok := h.receivedTimes[h.largestObserved]
	if !ok {
		return nil, errTimeLost
	}

	ackRanges := h.packetHistory.getAckRanges()
	h.currentSack = &sack{
		largestAcked:       h.largestObserved,
		largestInOrder:     ackRanges[len(ackRanges)-1].firstPacketNumber,
		packetReceivedTime: packetReceivedTime,
	}

	if len(ackRanges) > 1 {
		h.currentSack.ackRanges = ackRanges
	}

	return h.currentSack, nil
}

func (h *packetReceiver) garbageCollectReceivedTimes() {
	for i := h.lowestInReceivedTimes; i <= h.ignorePacketsBelow; i++ {
		delete(h.receivedTimes, i)
	}
	h.lowestInReceivedTimes = h.ignorePacketsBelow
}
