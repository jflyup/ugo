package ugo

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jflyup/ugo/ugo/utils"

	"github.com/jflyup/ugo/ugo/protocol"
)

type refBuffer struct {
	refcnt int32
	data   [256]byte
}

var bufferFreeList = &sync.Pool{
	New: func() interface{} { return new(refBuffer) },
}

func newRefBuffer() *refBuffer {
	buffer := bufferFreeList.Get().(*refBuffer)
	buffer.refcnt = 1
	return buffer
}

func (rb *refBuffer) addref() {
	atomic.AddInt32(&rb.refcnt, 1)
}

func (rb *refBuffer) release() {
	if atomic.AddInt32(&rb.refcnt, -1) == 0 {
		bufferFreeList.Put(rb)
	}
}

var (
	// ErrInvalidAckRanges occurs when a client sends inconsistent ACK ranges
	ErrInvalidAckRanges = errors.New("AckFrame: ACK frame contains invalid ACK ranges")
	// ErrInvalidFirstAckRange occurs when the first ACK range contains no packets
	ErrInvalidFirstAckRange = errors.New("AckFrame: ACK frame has invalid first ACK range")
)

var (
	errInconsistentAckLargestAcked = errors.New("internal inconsistency: LargestAcked does not match ACK ranges")
	errInconsistentAckLowestAcked  = errors.New("internal inconsistency: LowestAcked does not match ACK ranges")
)

type sackRange struct {
	FirstPacketNumber uint32
	LastPacketNumber  uint32
}

type sack struct {
	LargestAcked   uint32
	LargestInOrder uint32
	// has to be ordered.
	// The ACK range with the highest FirstPacketNumber goes first,
	// the ACK range with the lowest FirstPacketNumber goes last
	AckRanges          []sackRange
	DelayTime          time.Duration
	PacketReceivedTime time.Time
}

type segment struct {
	Offset uint32
	data   []byte
}

func parseSegment(r *bytes.Reader) (*segment, error) {
	frame := &segment{}

	var err error
	if frame.Offset, err = utils.ReadUint32(r); err != nil {
		return nil, err
	}
	l, err := utils.ReadUint16(r)
	if err != nil {
		return nil, err
	}

	frame.data = make([]byte, l)
	if _, err := r.Read(frame.data); err != nil {
		return nil, err
	}

	return frame, nil
}

func (s *segment) Write(b *bytes.Buffer) error {
	utils.WriteUint32(b, s.Offset)
	utils.WriteUint16(b, uint16(len(s.data)))
	b.Write(s.data)

	return nil
}

func (s *segment) DataLen() uint32 {
	return uint32(len(s.data))
}

/*
0        1                                5   ...  n                       n+4               n+6 ...
+--------+--------------------------------+--------+------------------------+----------------+-----
|  flag  |       packet number(opt)       |  SACK  |      offset            |  data length   | data
+--------+--------------------------------+--------+------------------------+----------------+-----
*/

// flag ack+stop+data+fin+rst+wnd

type ugoPacket struct {
	rawData       []byte
	flag          byte
	Length        uint32
	retransmitted bool
	sendTime      time.Time
	missingCount  uint8
	packetNumber  uint32 // TODO Varint
	stopWaiting   uint32
	sack          *sack
	segments      []*segment
}

func (p *ugoPacket) decode() error {
	r := bytes.NewReader(p.rawData)
	if flagByte, err := r.ReadByte(); err == nil {
		p.flag = flagByte
	} else {
		return err
	}

	if p.flag&0x80 == 0x80 { // ack flag
		ackFrame, err := parseSack(r)
		if err != nil {
			return err
		}
		p.sack = ackFrame
	}

	if p.flag != 0x80 {
		p.packetNumber, _ = utils.ReadUint32(r)
	} else {
		p.packetNumber = 0
	}

	if p.flag&0x40 == 0x40 { // largest in order
		p.stopWaiting, _ = utils.ReadUint32(r)
	}

	for r.Len() > 0 {
		s, err := parseSegment(r)
		if err != nil {
			return err
		}
		p.segments = append(p.segments, s)
	}

	return nil
}

func (p *ugoPacket) encode() error {
	buf := new(bytes.Buffer)

	if p.sack != nil {
		p.flag |= 0x80
	}

	if p.stopWaiting != 0 {
		p.flag |= 0x40
	}

	if len(p.segments) != 0 {
		p.flag |= 0x20
	}

	buf.WriteByte(p.flag)

	if p.sack != nil {
		if err := p.sack.Write(buf); err != nil {
			return err
		}
	}

	if p.flag != 0x80 {
		utils.WriteUint32(buf, p.packetNumber)
	}

	if p.stopWaiting != 0 {
		utils.WriteUint32(buf, p.stopWaiting)
	}

	for _, f := range p.segments {
		if err := f.Write(buf); err != nil {
			return err
		}
	}

	p.rawData = make([]byte, buf.Len())
	copy(p.rawData, buf.Bytes())
	p.Length = uint32(len(p.rawData))

	return nil
}

// parseSack reads an SACK
func parseSack(r *bytes.Reader) (*sack, error) {
	s := &sack{}

	typeByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}

	hasMissingRanges := false // length of the Missing Packet Sequence Number Delta
	if typeByte&0x20 == 0x20 {
		hasMissingRanges = true
	}

	largestAckedLen := 2 * ((typeByte & 0x0C) >> 2)
	if largestAckedLen == 0 {
		largestAckedLen = 1
	}

	missingSequenceNumberDeltaLen := 2 * (typeByte & 0x03)
	if missingSequenceNumberDeltaLen == 0 {
		missingSequenceNumberDeltaLen = 1
	}

	largestAcked, err := utils.ReadUintN(r, largestAckedLen)
	if err != nil {
		return nil, err
	}
	s.LargestAcked = uint32(largestAcked)

	delay, err := utils.ReadUfloat16(r)
	if err != nil {
		return nil, err
	}
	s.DelayTime = time.Duration(delay) * time.Microsecond

	var numAckBlocks uint8
	if hasMissingRanges {
		numAckBlocks, err = r.ReadByte()
		if err != nil {
			return nil, err
		}
	}

	if hasMissingRanges && numAckBlocks == 0 {
		return nil, ErrInvalidAckRanges
	}

	ackBlockLength, err := utils.ReadUintN(r, missingSequenceNumberDeltaLen)
	if err != nil {
		return nil, err
	}
	if ackBlockLength < 1 {
		return nil, ErrInvalidFirstAckRange
	}

	if ackBlockLength > largestAcked {
		return nil, ErrInvalidAckRanges
	}

	if hasMissingRanges {
		ackRange := sackRange{
			FirstPacketNumber: uint32(largestAcked-ackBlockLength) + 1,
			LastPacketNumber:  s.LargestAcked,
		}
		s.AckRanges = append(s.AckRanges, ackRange)

		var inLongBlock bool
		var lastRangeComplete bool
		for i := uint8(0); i < numAckBlocks; i++ {
			var gap uint8
			gap, err = r.ReadByte()
			if err != nil {
				return nil, err
			}

			ackBlockLength, err = utils.ReadUintN(r, missingSequenceNumberDeltaLen)
			if err != nil {
				return nil, err
			}

			length := uint32(ackBlockLength)

			if inLongBlock {
				s.AckRanges[len(s.AckRanges)-1].FirstPacketNumber -= uint32(gap) + length
				s.AckRanges[len(s.AckRanges)-1].LastPacketNumber -= uint32(gap)
			} else {
				lastRangeComplete = false
				ackRange := sackRange{
					LastPacketNumber: s.AckRanges[len(s.AckRanges)-1].FirstPacketNumber - uint32(gap) - 1,
				}
				ackRange.FirstPacketNumber = ackRange.LastPacketNumber - length + 1
				s.AckRanges = append(s.AckRanges, ackRange)
			}

			if length > 0 {
				lastRangeComplete = true
			}

			inLongBlock = (ackBlockLength == 0)
		}

		// if the last range was not complete, FirstPacketNumber and LastPacketNumber make no sense
		// remove the range from frame.AckRanges
		if !lastRangeComplete {
			s.AckRanges = s.AckRanges[:len(s.AckRanges)-1]
		}

		s.LargestInOrder = s.AckRanges[len(s.AckRanges)-1].FirstPacketNumber
	} else {
		s.LargestInOrder = uint32(largestAcked + 1 - ackBlockLength)
	}

	if !s.validateAckRanges() {
		return nil, ErrInvalidAckRanges
	}

	return s, nil
}

// Write writes an ACK frame.
func (s *sack) Write(b *bytes.Buffer) error {
	largestAckedLen := protocol.GetPacketNumberLength(protocol.PacketNumber(s.LargestAcked))

	typeByte := uint8(0x40)

	if largestAckedLen != protocol.PacketNumberLen1 {
		typeByte ^= (uint8(largestAckedLen / 2)) << 2
	}

	missingSequenceNumberDeltaLen := s.getMissingSequenceNumberDeltaLen()
	if missingSequenceNumberDeltaLen != protocol.PacketNumberLen1 {
		typeByte ^= (uint8(missingSequenceNumberDeltaLen / 2))
	}

	if s.HasMissingRanges() {
		typeByte |= 0x20
	}

	b.WriteByte(typeByte)

	switch largestAckedLen {
	case protocol.PacketNumberLen1:
		b.WriteByte(uint8(s.LargestAcked))
	case protocol.PacketNumberLen2:
		utils.WriteUint16(b, uint16(s.LargestAcked))
	case protocol.PacketNumberLen4:
		utils.WriteUint32(b, uint32(s.LargestAcked))
	case protocol.PacketNumberLen6:
		utils.WriteUint48(b, uint64(s.LargestAcked))
	}

	s.DelayTime = time.Now().Sub(s.PacketReceivedTime)
	utils.WriteUfloat16(b, uint64(s.DelayTime/time.Microsecond))

	var numRanges uint64
	var numRangesWritten uint64
	if s.HasMissingRanges() {
		numRanges = s.numWritableNackRanges()
		if numRanges > 0xFF {
			panic("AckFrame: Too many ACK ranges")
		}
		b.WriteByte(uint8(numRanges - 1))
	}

	var firstAckBlockLength uint32
	if !s.HasMissingRanges() {
		firstAckBlockLength = s.LargestAcked - s.LargestInOrder + 1
	} else {
		if s.LargestAcked != s.AckRanges[0].LastPacketNumber {
			return errInconsistentAckLargestAcked
		}
		if s.LargestInOrder != s.AckRanges[len(s.AckRanges)-1].FirstPacketNumber {
			fmt.Printf("largest in order: %d, sack: %v", s.LargestInOrder, s.AckRanges)
			return errInconsistentAckLowestAcked
		}
		firstAckBlockLength = s.LargestAcked - s.AckRanges[0].FirstPacketNumber + 1
		numRangesWritten++
	}

	switch missingSequenceNumberDeltaLen {
	case protocol.PacketNumberLen1:
		b.WriteByte(uint8(firstAckBlockLength))
	case protocol.PacketNumberLen2:
		utils.WriteUint16(b, uint16(firstAckBlockLength))
	case protocol.PacketNumberLen4:
		utils.WriteUint32(b, uint32(firstAckBlockLength))
	case protocol.PacketNumberLen6:
		utils.WriteUint48(b, uint64(firstAckBlockLength))
	}

	for i, ackRange := range s.AckRanges {
		if i == 0 {
			continue
		}

		length := ackRange.LastPacketNumber - ackRange.FirstPacketNumber + 1
		gap := s.AckRanges[i-1].FirstPacketNumber - ackRange.LastPacketNumber - 1

		num := gap/0xFF + 1
		if gap%0xFF == 0 {
			num--
		}

		if num == 1 {
			b.WriteByte(uint8(gap))
			switch missingSequenceNumberDeltaLen {
			case protocol.PacketNumberLen1:
				b.WriteByte(uint8(length))
			case protocol.PacketNumberLen2:
				utils.WriteUint16(b, uint16(length))
			case protocol.PacketNumberLen4:
				utils.WriteUint32(b, uint32(length))
			case protocol.PacketNumberLen6:
				utils.WriteUint48(b, uint64(length))
			}
			numRangesWritten++
		} else {
			for i := 0; i < int(num); i++ {
				var lengthWritten uint64
				var gapWritten uint8

				if i == int(num)-1 { // last block
					lengthWritten = uint64(length)
					gapWritten = uint8(gap % 0xFF)
				} else {
					lengthWritten = 0
					gapWritten = 0xFF
				}

				b.WriteByte(uint8(gapWritten))
				switch missingSequenceNumberDeltaLen {
				case protocol.PacketNumberLen1:
					b.WriteByte(uint8(lengthWritten))
				case protocol.PacketNumberLen2:
					utils.WriteUint16(b, uint16(lengthWritten))
				case protocol.PacketNumberLen4:
					utils.WriteUint32(b, uint32(lengthWritten))
				case protocol.PacketNumberLen6:
					utils.WriteUint48(b, uint64(lengthWritten))
				}

				numRangesWritten++
			}
		}

		// this is needed if not all AckRanges can be written to the ACK frame (if there are more than 0xFF)
		if numRangesWritten >= numRanges {
			break
		}
	}

	if numRanges != numRangesWritten {
		log.Println("illegal ack: ", s)
		return errors.New("BUG: Inconsistent number of ACK ranges written")
	}

	return nil
}

func (s *sack) HasMissingRanges() bool {
	if len(s.AckRanges) > 0 {
		return true
	}
	return false
}

func (s *sack) validateAckRanges() bool {
	if len(s.AckRanges) == 0 {
		return true
	}

	// if there are missing packets, there will always be at least 2 ACK ranges
	if len(s.AckRanges) == 1 {
		return false
	}

	if s.AckRanges[0].LastPacketNumber != s.LargestAcked {
		return false
	}

	// check the validity of every single ACK range
	for _, ackRange := range s.AckRanges {
		if ackRange.FirstPacketNumber > ackRange.LastPacketNumber {
			return false
		}
	}

	// check the consistency for ACK with multiple NACK ranges
	for i, ackRange := range s.AckRanges {
		if i == 0 {
			continue
		}
		lastAckRange := s.AckRanges[i-1]
		if lastAckRange.FirstPacketNumber <= ackRange.FirstPacketNumber {
			return false
		}
		if lastAckRange.FirstPacketNumber <= ackRange.LastPacketNumber+1 {
			return false
		}
	}

	return true
}

// numWritableNackRanges calculates the number of ACK blocks that are about to be written
// this number is different from len(f.AckRanges) for the case of long gaps (> 255 packets)
func (s *sack) numWritableNackRanges() uint64 {
	if len(s.AckRanges) == 0 {
		return 0
	}

	var numRanges uint64
	for i, ackRange := range s.AckRanges {
		if i == 0 {
			continue
		}

		lastAckRange := s.AckRanges[i-1]
		gap := lastAckRange.FirstPacketNumber - ackRange.LastPacketNumber
		rangeLength := uint64(gap) / (0xFF + 1)
		if uint64(gap)%(0xFF+1) != 0 {
			rangeLength++
		}

		if numRanges+rangeLength < 0xFF {
			numRanges += rangeLength
		} else {
			break
		}
	}

	return numRanges + 1
}

func (s *sack) getMissingSequenceNumberDeltaLen() protocol.PacketNumberLen {
	var maxRangeLength uint32

	if s.HasMissingRanges() {
		for _, ackRange := range s.AckRanges {
			rangeLength := ackRange.LastPacketNumber - ackRange.FirstPacketNumber + 1
			if rangeLength > maxRangeLength {
				maxRangeLength = rangeLength
			}
		}
	} else {
		maxRangeLength = s.LargestAcked - s.LargestInOrder + 1
	}

	if maxRangeLength <= 0xFF {
		return protocol.PacketNumberLen1
	}
	if maxRangeLength <= 0xFFFF {
		return protocol.PacketNumberLen2
	}
	if maxRangeLength <= 0xFFFFFFFF {
		return protocol.PacketNumberLen4
	}

	return protocol.PacketNumberLen6
}
