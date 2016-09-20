package ugo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jflyup/ugo/ugo/utils"
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

const (
	ackFlag  byte = 0x80
	stopFlag byte = 0x40
	pshFlag  byte = 0x20
	finFlag  byte = 0x10
)

type sackRange struct {
	firstPacketNumber uint64
	lastPacketNumber  uint64
}

type sack struct {
	largestAcked   uint64
	largestInOrder uint64
	// has to be ordered.
	// The ACK range with the highest FirstPacketNumber goes first,
	// the ACK range with the lowest FirstPacketNumber goes last
	ackRanges          []sackRange
	delayTime          time.Duration
	packetReceivedTime time.Time
}

type segment struct {
	offset uint64
	data   []byte
}

func parseSegment(r *bytes.Reader) (*segment, error) {
	frame := &segment{}

	var err error
	if frame.offset, err = binary.ReadUvarint(r); err != nil {
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
	numBuf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(numBuf, s.offset)
	b.Write(numBuf[:n])

	utils.WriteUint16(b, uint16(len(s.data)))
	b.Write(s.data)

	return nil
}

func (s *segment) DataLen() uint64 {
	return uint64(len(s.data))
}

/*
0        1            varint                                varint                 uint16
+--------+--------------------------------+--------+------------------------+----------------+-----
|  flags  |       packet number(opt)       |  SACK  |      offset            |  data length   | data
+--------+--------------------------------+--------+------------------------+----------------+-----
*/

// flags ack+stop+psh+fin+rst+wnd

type ugoPacket struct {
	rawData       []byte
	flags         byte
	Length        uint32
	retransmitted bool
	sendTime      time.Time
	missingCount  uint8
	packetNumber  uint64
	stopWaiting   uint64
	sack          *sack
	segments      []*segment
}

func (p *ugoPacket) decode() (err error) {
	r := bytes.NewReader(p.rawData)
	if p.flags, err = r.ReadByte(); err != nil {
		return
	}

	if p.flags&ackFlag == ackFlag { // ack flag
		p.sack, err = parseSack(r)
		if err != nil {
			return
		}
	}

	if p.flags != ackFlag {
		if p.packetNumber, err = binary.ReadUvarint(r); err != nil {
			return err
		}
	} else {
		p.packetNumber = 0
	}

	if p.flags&stopFlag == stopFlag {
		if p.stopWaiting, err = binary.ReadUvarint(r); err != nil {
			return err
		}
	}

	for r.Len() > 0 {
		s, e := parseSegment(r)
		if e != nil {
			return e
		}
		p.segments = append(p.segments, s)
	}

	return nil
}

// func (b *bytes.Buffer) writeUvarint(n uint64) {
// 	numBuf := make([]byte, binary.MaxVarintLen64)
// 	pos := binary.PutUvarint(numBuf, n)
// 	b.Write(numBuf[:pos])
// }

func (p *ugoPacket) encode() error {
	buf := new(bytes.Buffer)

	if p.sack != nil {
		p.flags |= ackFlag
	}

	if p.stopWaiting != 0 {
		p.flags |= stopFlag
	}

	if len(p.segments) != 0 {
		p.flags |= pshFlag
	}

	buf.WriteByte(p.flags)

	if p.sack != nil {
		if err := p.sack.Write(buf); err != nil {
			return err
		}
	}

	if p.flags != ackFlag {
		numBuf := make([]byte, binary.MaxVarintLen64)
		n := binary.PutUvarint(numBuf, p.packetNumber)
		buf.Write(numBuf[:n])
	}

	if p.stopWaiting != 0 {
		numBuf := make([]byte, binary.MaxVarintLen64)
		n := binary.PutUvarint(numBuf, p.stopWaiting)
		buf.Write(numBuf[:n])
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

// parseSack parses an SACK
func parseSack(r *bytes.Reader) (*sack, error) {
	s := &sack{}

	typeByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}

	hasMissingRanges := false
	if typeByte&0x20 == 0x20 {
		hasMissingRanges = true
	}

	largestAcked, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	s.largestAcked = largestAcked

	delay, err := utils.ReadUfloat16(r)
	if err != nil {
		return nil, err
	}
	s.delayTime = time.Duration(delay) * time.Microsecond

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

	ackBlockLength, err := binary.ReadUvarint(r)
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
			firstPacketNumber: (largestAcked - ackBlockLength) + 1,
			lastPacketNumber:  s.largestAcked,
		}
		s.ackRanges = append(s.ackRanges, ackRange)

		var inLongBlock bool
		var lastRangeComplete bool
		for i := uint8(0); i < numAckBlocks; i++ {
			var gap uint8
			gap, err = r.ReadByte()
			if err != nil {
				return nil, err
			}

			ackBlockLength, err = binary.ReadUvarint(r)
			if err != nil {
				return nil, err
			}

			if inLongBlock {
				s.ackRanges[len(s.ackRanges)-1].firstPacketNumber -= uint64(gap) + ackBlockLength
				s.ackRanges[len(s.ackRanges)-1].lastPacketNumber -= uint64(gap)
			} else {
				lastRangeComplete = false
				ackRange := sackRange{
					lastPacketNumber: s.ackRanges[len(s.ackRanges)-1].firstPacketNumber - uint64(gap) - 1,
				}
				ackRange.firstPacketNumber = ackRange.lastPacketNumber - ackBlockLength + 1
				s.ackRanges = append(s.ackRanges, ackRange)
			}

			if ackBlockLength > 0 {
				lastRangeComplete = true
			}

			inLongBlock = (ackBlockLength == 0)
		}

		// if the last range was not complete, FirstPacketNumber and LastPacketNumber make no sense
		// remove the range from frame.AckRanges
		if !lastRangeComplete {
			s.ackRanges = s.ackRanges[:len(s.ackRanges)-1]
		}

		s.largestInOrder = uint64(s.ackRanges[len(s.ackRanges)-1].firstPacketNumber)
	} else {
		s.largestInOrder = largestAcked + 1 - ackBlockLength
	}

	if !s.validateAckRanges() {
		return nil, ErrInvalidAckRanges
	}

	return s, nil
}

// Write writes an ACK frame.
func (s *sack) Write(b *bytes.Buffer) error {
	var typeByte uint8

	if s.HasMissingRanges() {
		typeByte |= 0x20
	}

	b.WriteByte(typeByte)
	numBuf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(numBuf, s.largestAcked)
	b.Write(numBuf[:n])

	s.delayTime = time.Now().Sub(s.packetReceivedTime)
	utils.WriteUfloat16(b, uint64(s.delayTime/time.Microsecond))

	var numRanges uint64
	var numRangesWritten uint64
	if s.HasMissingRanges() {
		numRanges = s.numWritableNackRanges()
		if numRanges > 0xFF {
			panic("AckFrame: Too many ACK ranges")
		}
		b.WriteByte(uint8(numRanges - 1))
	}

	var firstAckBlockLength uint64
	if !s.HasMissingRanges() {
		firstAckBlockLength = s.largestAcked - s.largestInOrder + 1
	} else {
		if s.largestAcked != s.ackRanges[0].lastPacketNumber {
			return errInconsistentAckLargestAcked
		}
		if s.largestInOrder != s.ackRanges[len(s.ackRanges)-1].firstPacketNumber {
			fmt.Printf("largest in order: %d, sack: %v", s.largestInOrder, s.ackRanges)
			return errInconsistentAckLowestAcked
		}
		firstAckBlockLength = s.largestAcked - s.ackRanges[0].firstPacketNumber + 1
		numRangesWritten++
	}

	n = binary.PutUvarint(numBuf, firstAckBlockLength)
	b.Write(numBuf[:n])

	for i, ackRange := range s.ackRanges {
		if i == 0 {
			continue
		}

		length := ackRange.lastPacketNumber - ackRange.firstPacketNumber + 1
		gap := s.ackRanges[i-1].firstPacketNumber - ackRange.lastPacketNumber - 1

		num := gap/0xFF + 1
		if gap%0xFF == 0 {
			num--
		}

		if num == 1 {
			b.WriteByte(uint8(gap))
			n = binary.PutUvarint(numBuf, length)
			b.Write(numBuf[:n])
			numRangesWritten++
		} else {
			for i := 0; i < int(num); i++ {
				var lengthWritten uint64
				var gapWritten uint8

				if i == int(num)-1 { // last block
					lengthWritten = length
					gapWritten = uint8(gap % 0xFF)
				} else {
					lengthWritten = 0
					gapWritten = 0xFF
				}

				b.WriteByte(uint8(gapWritten))
				n = binary.PutUvarint(numBuf, lengthWritten)
				b.Write(numBuf[:n])

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
	if len(s.ackRanges) > 0 {
		return true
	}
	return false
}

func (s *sack) validateAckRanges() bool {
	if len(s.ackRanges) == 0 {
		return true
	}

	// if there are missing packets, there will always be at least 2 ACK ranges
	if len(s.ackRanges) == 1 {
		return false
	}

	if s.ackRanges[0].lastPacketNumber != s.largestAcked {
		return false
	}

	// check the validity of every single ACK range
	for _, ackRange := range s.ackRanges {
		if ackRange.firstPacketNumber > ackRange.lastPacketNumber {
			return false
		}
	}

	// check the consistency for ACK with multiple NACK ranges
	for i, ackRange := range s.ackRanges {
		if i == 0 {
			continue
		}
		lastAckRange := s.ackRanges[i-1]
		if lastAckRange.firstPacketNumber <= ackRange.firstPacketNumber {
			return false
		}
		if lastAckRange.firstPacketNumber <= ackRange.lastPacketNumber+1 {
			return false
		}
	}

	return true
}

// numWritableNackRanges calculates the number of ACK blocks that are about to be written
// this number is different from len(f.AckRanges) for the case of long gaps (> 255 packets)
func (s *sack) numWritableNackRanges() uint64 {
	if len(s.ackRanges) == 0 {
		return 0
	}

	var numRanges uint64
	for i, ackRange := range s.ackRanges {
		if i == 0 {
			continue
		}

		lastAckRange := s.ackRanges[i-1]
		gap := lastAckRange.firstPacketNumber - ackRange.lastPacketNumber
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
