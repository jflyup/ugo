package ugo

import (
	"errors"
	"log"
	"time"

	"github.com/jflyup/ugo/ugo/congestion"
	"github.com/jflyup/ugo/ugo/utils"
)

var (
	// errDuplicateOrOutOfOrderAck occurs when a duplicate or an out-of-order ACK is received
	errDuplicateOrOutOfOrderAck = errors.New("packetSender: Duplicate or out-of-order ACK")
	// errTooManyTrackedSentPackets occurs when the packetSender has to keep track of too many packets
	errTooManyTrackedSentPackets = errors.New("Too many outstanding non-acked and non-retransmitted packets")
	errAckForUnsentPacket        = errors.New("Received ACK for an unsent packet")
	errDuplicatePacketNumber     = errors.New("Packet number already exists in Packet History")
	errConnectionFailures        = errors.New("peer not responding")
)

type packetSender struct {
	lastSentPacketNumber uint64
	lastSentPacketTime   time.Time
	largestInOrderAcked  uint64
	largestAcked         uint64

	largestReceivedPacketWithAck uint64

	packetHistory      map[uint64]*ugoPacket
	stopWaitingManager stopWaitingManager

	retransmissionQueue []*ugoPacket

	bytesInFlight       uint32
	consecutiveRTOCount uint32

	rttStats   *congestion.RTTStats
	congestion congestion.SendAlgorithm

	// for debug
	totalSend  uint32
	totalAcked uint32
}

// newPacketSender creates a new packetSender
func newPacketSender() *packetSender {
	rttStats := &congestion.RTTStats{}

	congestion := congestion.NewCubicSender(
		congestion.DefaultClock{},
		rttStats,
		false, /* don't use reno since chromium doesn't (why?) */
		initialCongestionWindow,
		defaultMaxCongestionWindow,
	)

	return &packetSender{
		packetHistory:      make(map[uint64]*ugoPacket),
		stopWaitingManager: stopWaitingManager{},
		rttStats:           rttStats,
		congestion:         congestion,
	}
}

func (h *packetSender) ackPacket(packetNumber uint64) *ugoPacket {
	if packetNumber != 0 {
		packet, ok := h.packetHistory[packetNumber]
		if ok && !packet.retransmitted {
			// if the packet is marked as retransmitted and exists in packetHistory,
			// it means this packet is queued for retransmission,
			// but an ACK for it comes before resending

			h.bytesInFlight -= packet.Length
			h.totalAcked += packet.Length
		}

		if h.largestInOrderAcked == packetNumber-1 {
			h.largestInOrderAcked++

			// update stop waiting
			h.stopWaitingManager.largestLeastUnackedSent = h.largestInOrderAcked + 1
		}

		delete(h.packetHistory, packetNumber)

		return packet
	}
	return nil
}

func (h *packetSender) nackPacket(packetNumber uint64) (*ugoPacket, error) {
	packet, ok := h.packetHistory[packetNumber]
	// This means that the packet has already been retransmitted, do nothing.
	// another NACK for this packet may come because the
	// retransmission has not yet arrived at the client.
	if !ok {
		return nil, nil
	}

	packet.missingCount++

	if packet.missingCount > retransmissionThreshold && !packet.retransmitted {
		log.Printf("fast retransimition packet %d", packet.packetNumber)
		h.queuePacketForRetransmission(packet) // fast retransmition
		return packet, nil
	}
	return nil, nil
}

func (h *packetSender) queuePacketForRetransmission(packet *ugoPacket) {
	h.bytesInFlight -= packet.Length
	h.retransmissionQueue = append(h.retransmissionQueue, packet)
	packet.retransmitted = true

	// the retransmitted packet will be removed from history when dequeueing

	// increase the LargestInOrderAcked, if this is the lowest packet that hasn't been acked yet
	if packet.packetNumber == h.largestInOrderAcked+1 {
		h.largestInOrderAcked++
		for i := h.largestInOrderAcked + 1; i <= h.largestAcked; i++ {
			_, ok := h.packetHistory[i]
			if !ok {
				h.largestInOrderAcked = i
			} else {
				break
			}
		}
	}

	// send stopWaiting only when restransmisson happened
	h.stopWaitingManager.SetBoundary(h.largestInOrderAcked)
}

func (h *packetSender) sentPacket(packet *ugoPacket) error {
	if packet.flags != ackFlag {
		_, ok := h.packetHistory[packet.packetNumber]
		if ok {
			return errDuplicatePacketNumber
		}

		now := time.Now()
		h.lastSentPacketTime = now
		packet.sendTime = now
		if packet.Length == 0 {
			return errors.New("packetSender: packet cannot be empty")
		}

		h.lastSentPacketNumber = packet.packetNumber
		h.totalSend += packet.Length
		h.bytesInFlight += packet.Length
		h.packetHistory[packet.packetNumber] = packet

		h.congestion.OnPacketSent(
			time.Now(),
			h.BytesInFlight(),
			packet.packetNumber,
			packet.Length,
			true, /* TODO: is retransmittable */
		)
	}
	return nil
}

func (h *packetSender) receivedAck(ack *sack, withPacketNumber uint64) error {
	if ack.largestAcked > h.lastSentPacketNumber {
		return errAckForUnsentPacket
	}

	// duplicate or out-of-order ACK
	if withPacketNumber != 0 {
		if withPacketNumber <= h.largestReceivedPacketWithAck {
			return errDuplicateOrOutOfOrderAck
		}
	}

	if withPacketNumber != 0 {
		h.largestReceivedPacketWithAck = withPacketNumber
	}

	// ignore repeated or delayed ACK (ACKs that don't have a higher LargestAcked than the last ACK)
	if ack.largestAcked <= h.largestInOrderAcked {
		return nil
	}

	// out-of-order ACK
	if ack.largestAcked <= h.largestAcked {
		return nil
	}

	h.largestAcked = ack.largestAcked

	rttUpdated := false

	packet, ok := h.packetHistory[h.largestAcked]
	if ok {
		// Update the RTT
		rttUpdated = true
		timeDelta := time.Now().Sub(packet.sendTime)
		h.rttStats.UpdateRTT(timeDelta, ack.delayTime, time.Now())
		h.consecutiveRTOCount = 0

		log.Printf("Estimated RTT: %dms", h.rttStats.SmoothedRTT()/time.Millisecond)
	}

	var ackedPackets congestion.PacketVector
	var lostPackets congestion.PacketVector

	// in ideal condition, h.largestInOrderAcked should be equal with ack.LargestInOrder,
	// it not, it means newest ACK lost or out-of-order, delayed ACK or RTO
	for i := h.largestInOrderAcked; i < ack.largestInOrder; i++ {
		p := h.ackPacket(i)
		if p != nil {
			ackedPackets = append(ackedPackets, congestion.PacketInfo{Number: p.packetNumber, Length: p.Length})
		}
	}

	ackRangeIndex := 0
	for i := ack.largestInOrder; i <= ack.largestAcked; i++ {
		if ack.hasMissingRanges() {
			ackRange := ack.ackRanges[len(ack.ackRanges)-1-ackRangeIndex]

			if i > ackRange.lastPacketNumber && ackRangeIndex < len(ack.ackRanges)-1 {
				ackRangeIndex++
				ackRange = ack.ackRanges[len(ack.ackRanges)-1-ackRangeIndex]
			}

			if i >= ackRange.firstPacketNumber { // packet i contained in ACK range
				p := h.ackPacket(i)
				if p != nil {
					ackedPackets = append(ackedPackets, congestion.PacketInfo{Number: p.packetNumber, Length: p.Length})
				}
			} else {
				p, err := h.nackPacket(i)
				if err != nil {
					return err
				}
				if p != nil {
					lostPackets = append(lostPackets, congestion.PacketInfo{Number: p.packetNumber, Length: p.Length})
				}
			}
		} else {
			p := h.ackPacket(i)
			if p != nil {
				ackedPackets = append(ackedPackets, congestion.PacketInfo{Number: p.packetNumber, Length: p.Length})
			}
		}
	}

	log.Printf("largest in order send %d, ack in order %d", h.largestInOrderAcked, ack.largestInOrder)

	h.congestion.OnCongestionEvent(
		rttUpdated,
		h.BytesInFlight(),
		ackedPackets,
		lostPackets,
	)

	return nil
}

func (h *packetSender) dequeuePacketForRetransmission() (packet *ugoPacket) {
	if len(h.retransmissionQueue) == 0 {
		return nil
	}

	for len(h.retransmissionQueue) > 0 {
		queueLen := len(h.retransmissionQueue)
		// packets are usually NACKed in descending order. So use the slice as a stack
		packet = h.retransmissionQueue[queueLen-1]
		h.retransmissionQueue = h.retransmissionQueue[:queueLen-1]

		// this happens if a belated ACK arrives for this packet
		// no need to retransmit it
		_, ok := h.packetHistory[packet.packetNumber]
		if !ok {
			continue
		}

		delete(h.packetHistory, packet.packetNumber)
		return packet
	}

	return nil
}

func (h *packetSender) BytesInFlight() uint32 {
	return h.bytesInFlight
}

func (h *packetSender) GetLargestAcked() uint64 {
	return h.largestAcked
}

func (h *packetSender) GetStopWaitingFrame() uint64 {
	return h.stopWaitingManager.GetStopWaitingFrame(false)
}

func (h *packetSender) CongestionAllowsSending() bool {
	return h.BytesInFlight() <= h.congestion.GetCongestionWindow()
}

func (h *packetSender) CheckForError() error {
	length := len(h.retransmissionQueue) + len(h.packetHistory)
	if length > maxTrackedSentPackets {
		return errTooManyTrackedSentPackets
	}
	if h.consecutiveRTOCount > maxRetriesAttempted {
		return errConnectionFailures
	}
	return nil
}

func (h *packetSender) checkRTO() {
	if time.Now().Before(h.timeOfFirstRTO()) {
		return
	}

	for p := h.largestInOrderAcked + 1; p <= h.lastSentPacketNumber; p++ {
		packet := h.packetHistory[p]
		if packet != nil && !packet.retransmitted {
			packetsLost := congestion.PacketVector{congestion.PacketInfo{
				Number: packet.packetNumber,
				Length: packet.Length,
			}}

			h.congestion.OnCongestionEvent(false, h.BytesInFlight(), nil, packetsLost)
			// in Reno algorithm, if an ACK times out (RTO timeout),
			// slow start is used, reduce congestion window to 1 MSS
			h.congestion.OnRetransmissionTimeout(true)
			log.Printf("retransmission timeout, packet %d, time delta: %dms", packet.packetNumber, time.Now().Sub(packet.sendTime).Nanoseconds()/int64(time.Millisecond))
			h.queuePacketForRetransmission(packet)
			// reset RTO timer since this packet does not always get transmited immediately.
			// After a RTO, congestion window may not allow to send
			h.lastSentPacketTime = time.Now()
			h.consecutiveRTOCount++
			return
		}
	}
}

func (h *packetSender) getRTO() time.Duration {
	rto := h.congestion.RetransmissionDelay()
	if rto == 0 {
		rto = defaultRetransmissionTime
	}
	rto = utils.MaxDuration(rto, minRetransmissionTime)
	// Exponential backoff
	rto *= 1 << h.consecutiveRTOCount
	return utils.MinDuration(rto, maxRetransmissionTime)
}

func (h *packetSender) timeOfFirstRTO() time.Time {
	if h.lastSentPacketTime.IsZero() {
		return time.Time{}
	}
	return h.lastSentPacketTime.Add(h.getRTO())
}
