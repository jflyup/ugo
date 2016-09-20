package ugo

import (
	"errors"
	"log"
	//"sort"
	"time"

	"github.com/jflyup/ugo/ugo/congestion"
	"github.com/jflyup/ugo/ugo/utils"
)

var (
	// ErrDuplicateOrOutOfOrderAck occurs when a duplicate or an out-of-order ACK is received
	errDuplicateOrOutOfOrderAck = errors.New("packetSender: Duplicate or out-of-order ACK")
	// errTooManyTrackedSentPackets occurs when the packetSender has to keep track of too many packets
	errTooManyTrackedSentPackets = errors.New("Too many outstanding non-acked and non-retransmitted packets")
	errAckForUnsentPacket        = errors.New("Received ACK for an unsent packet")
	errDuplicatePacketNumber     = errors.New("Packet number already exists in Packet History")
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

	bytesInFlight uint32

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
		InitialCongestionWindow,
		DefaultMaxCongestionWindow,
	)

	return &packetSender{
		packetHistory:      make(map[uint64]*ugoPacket),
		stopWaitingManager: stopWaitingManager{},
		rttStats:           rttStats,
		congestion:         congestion,
	}
}

func (h *packetSender) ackPacket(packetNumber uint64) *ugoPacket {
	packet, ok := h.packetHistory[packetNumber]
	if ok && !packet.retransmitted {
		// if the packet is marked as retransmitted and it exist in packetHistory,
		// it means this packet is queued for retransmission,
		// but ACK for it comes before resending
		if h.bytesInFlight < packet.Length {
			log.Println("BUG: bytes in flight < 0")
			h.bytesInFlight = 0
			h.totalAcked += packet.Length
		} else {
			h.bytesInFlight -= packet.Length
			h.totalAcked += packet.Length
		}
	}

	if h.largestInOrderAcked == packetNumber-1 {
		h.largestInOrderAcked++

		// update stop waiting
		h.stopWaitingManager.largestLeastUnackedSent = h.largestInOrderAcked + 1
	}

	delete(h.packetHistory, packetNumber)

	return packet
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

	if packet.missingCount > 3 && !packet.retransmitted {
		log.Printf("fast retransimition packet %d, Missing count %d", packet.packetNumber, packet.missingCount)
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
			_, ok := h.packetHistory[uint64(i)]
			if !ok {
				h.largestInOrderAcked = i
			} else {
				break
			}
		}
	}

	log.Printf("retransfer packet %d, flag: %d, length %d", packet.packetNumber, packet.flags, packet.Length)

	// send stopWaiting only when restransmisson happened
	h.stopWaitingManager.SetBoundary(h.largestInOrderAcked)
}

func (h *packetSender) SentPacket(packet *ugoPacket) error {
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
	if packet.flags != 0x80 {
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

func (h *packetSender) ReceivedAck(ack *sack, withPacketNumber uint64) error {
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

	packet, ok := h.packetHistory[h.largestAcked]
	if ok {
		// Update the RTT
		timeDelta := time.Now().Sub(packet.sendTime)
		// TODO: Don't always update RTT
		h.rttStats.UpdateRTT(timeDelta, ack.delayTime, time.Now())

		log.Printf("Estimated RTT: %dms", h.rttStats.SmoothedRTT()/time.Millisecond)

	}

	var ackedPackets congestion.PacketVector
	var lostPackets congestion.PacketVector

	// in ideal condition, h.largestInOrderAcked should be equal with ack.LargestInOrder,
	// it not, it means newest ACK lost or out-of-order/delayed ACK
	for i := h.largestInOrderAcked; i < ack.largestInOrder; i++ {
		p := h.ackPacket(i)
		if p != nil {
			ackedPackets = append(ackedPackets, congestion.PacketInfo{Number: p.packetNumber, Length: p.Length})
		}
	}

	ackRangeIndex := 0
	for i := ack.largestInOrder; i <= ack.largestAcked; i++ {
		if ack.HasMissingRanges() {
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
		true, /* TODO: rtt updated */
		h.BytesInFlight(),
		ackedPackets,
		lostPackets,
	)

	log.Printf("sent %d, acked %d, history size: %d", h.totalSend, h.totalAcked, len(h.packetHistory))

	return nil
}

// checkRetransmission returns if there is a packet queued for retransmission
// There is one case where it gets the answer wrong:
// if a packet has already been queued for retransmission,
// but a belated ACK is received for this packet, this function will return true,
// although the packet will not be returend for retransmission by DequeuePacketForRetransmission()
func (h *packetSender) checkRetransmission() bool {
	h.checkPacketTimeout()

	return len(h.retransmissionQueue) > 0
}

func (h *packetSender) DequeuePacketForRetransmission() (packet *ugoPacket) {
	if !h.checkRetransmission() {
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
	if length > 2000 {
		log.Printf("retransmissionQueue size: %d, history size: %d", len(h.retransmissionQueue), len(h.packetHistory))
		return errTooManyTrackedSentPackets
	}
	return nil
}

func (h *packetSender) checkPacketTimeout() {
	if time.Now().Before(h.TimeOfFirstRTO()) {
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
			h.congestion.OnRetransmissionTimeout(true)
			log.Printf("timeout retransmission, packet %d, send time:%s, now: %s", packet.packetNumber, packet.sendTime.String(), time.Now().String())
			h.queuePacketForRetransmission(packet)
			return
		}
	}
}

func (h *packetSender) getRTO() time.Duration {
	rto := h.congestion.RetransmissionDelay()
	if rto == 0 {
		rto = defaultRetransmissionTime
	}
	return utils.MaxDuration(rto, minRetransmissionTime)
}

func (h *packetSender) TimeOfFirstRTO() time.Time {
	if h.lastSentPacketTime.IsZero() {
		return time.Time{}
	}
	return h.lastSentPacketTime.Add(h.getRTO())
}
