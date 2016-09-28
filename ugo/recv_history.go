package ugo

import (
	"sync"

	"github.com/jflyup/ugo/ugo/utils"
)

type recvHistory struct {
	// TODO use interval tree instead
	ranges *utils.PacketIntervalList

	mutex sync.RWMutex
}

// newRecvHistory creates a new received packet history
func newRecvHistory() *recvHistory {
	return &recvHistory{
		ranges: utils.NewPacketIntervalList(),
	}
}

// receivedPacket registers a packet with PacketNumber p and updates the ranges
func (h *recvHistory) receivedPacket(p uint64) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if h.ranges.Len() == 0 {
		h.ranges.PushBack(utils.PacketInterval{Start: p, End: p})
		// this must be the first time we receive packet,
		// if it's not packet 1, insert a empty range,
		if p != 1 {
			h.ranges.PushFront(utils.PacketInterval{Start: 0, End: 0})
		}
		return
	}

	for el := h.ranges.Back(); el != nil; el = el.Prev() {
		// p already included in an existing range. Nothing to do here
		if p >= el.Value.Start && p <= el.Value.End {
			return
		}

		var rangeExtended bool
		if el.Value.End == p-1 { // extend a range at the end
			rangeExtended = true
			el.Value.End = p
		} else if el.Value.Start == p+1 { // extend a range at the beginning
			rangeExtended = true
			el.Value.Start = p
		}

		// if a range was extended (either at the beginning or at the end, maybe it is possible to merge two ranges into one)
		if rangeExtended {
			prev := el.Prev()
			if prev != nil && prev.Value.End+1 == el.Value.Start { // merge two ranges
				prev.Value.End = el.Value.End
				h.ranges.Remove(el)
				return
			}
			return // if the two ranges didn't merge, we're done here
		}

		// create a new range at the end
		if p > el.Value.End {
			h.ranges.InsertAfter(utils.PacketInterval{Start: p, End: p}, el)
			return
		}
	}

	// create a new range at the beginning
	h.ranges.InsertBefore(utils.PacketInterval{Start: p, End: p}, h.ranges.Front())
}

func (h *recvHistory) deleteBelow(leastUnacked uint64) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	nextEl := h.ranges.Front()
	for el := h.ranges.Front(); nextEl != nil; el = nextEl {
		nextEl = el.Next()

		if leastUnacked >= el.Value.Start && leastUnacked <= el.Value.End {
			el.Value.Start = leastUnacked
		}
		if el.Value.End < leastUnacked { // delete a whole range
			h.ranges.Remove(el)
			// handle leastUnacked in the gap, for example, [10,8] [5,5], recv stopWaiting 7
			if nextEl != nil && nextEl.Value.Start > leastUnacked {
				h.ranges.InsertBefore(utils.PacketInterval{Start: leastUnacked, End: leastUnacked}, nextEl)
			} else if nextEl == nil {
				// if leastUnacked > largestObserved, create a new range
				h.ranges.PushBack(utils.PacketInterval{Start: leastUnacked, End: leastUnacked})
			}
		} else {
			return
		}
	}
}

// getAckRanges gets a slice of all AckRanges that can be used in an SACK
func (h *recvHistory) getAckRanges() []ackRange {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	if h.ranges.Len() == 0 {
		return nil
	}

	var ackRanges []ackRange

	for el := h.ranges.Back(); el != nil; el = el.Prev() {
		ackRanges = append(ackRanges, ackRange{firstPacketNumber: el.Value.Start, lastPacketNumber: el.Value.End})
	}

	return ackRanges
}
