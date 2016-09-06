package ugo

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"./utils"

	"./protocol"
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

type AckRange struct {
	FirstPacketNumber uint32
	LastPacketNumber  uint32
}

type AckFrame struct {
	LargestAcked uint32
	//LowestAcked    uint32
	LargestInOrder uint32
	AckRanges      []AckRange // has to be ordered. The ACK range with the highest FirstPacketNumber goes first, the ACK range with the lowest FirstPacketNumber goes last

	DelayTime          time.Duration
	PacketReceivedTime time.Time // only for received packets. Will not be modified for received ACKs frames
}

type segment struct {
	Offset uint32
	Data   []byte
	FinBit bool
}

func parseSegment(r *bytes.Reader) (*segment, error) {
	frame := &segment{}
	frame.FinBit = false // TODO

	var err error
	if frame.Offset, err = utils.ReadUint32(r); err != nil {
		return nil, err
	}
	if dataLen, err := utils.ReadUint16(r); err != nil {
		return nil, err
	} else {
		frame.Data = make([]byte, dataLen)
		if _, err := r.Read(frame.Data); err != nil {
			return nil, err
		}
	}

	return frame, nil
}

func (f *segment) Write(b *bytes.Buffer) error {
	utils.WriteUint32(b, f.Offset)
	utils.WriteUint16(b, uint16(len(f.Data)))
	b.Write(f.Data)

	return nil
}

func (f *segment) DataLen() uint32 {
	return uint32(len(f.Data))
}

/*
0        1                                5   ...  n                       n+4               n+6 ...
+--------+--------------------------------+--------+------------------------+----------------+-----
|  flag  |       packet number            |  SACK  |      offset            |  data length   | data
+--------+--------------------------------+--------+------------------------+----------------+-----
*/

// flag ack+stop+data+fin+rst+wnd

type Packet struct {
	D              []byte
	flag           byte
	Length         uint32
	Retransmitted  bool
	SendTime       time.Time
	MissingReports uint8
	PacketNumber   uint32 // TODO Varint
	stopWaiting    uint32
	ackFrame       *AckFrame
	segments       []*segment
}

func (p *Packet) decode() error {
	r := bytes.NewReader(p.D)
	if flagByte, err := r.ReadByte(); err == nil {
		p.flag = flagByte
	} else {
		return err
	}

	if p.flag&0x80 == 0x80 { // ack flag
		if ackFrame, err := ParseAckFrame(r); err != nil {
			return err
		} else {
			p.ackFrame = ackFrame
		}
	}

	if p.flag != 0x80 {
		p.PacketNumber, _ = utils.ReadUint32(r)
	} else {
		p.PacketNumber = 0
	}

	if p.flag&0x40 == 0x40 { // largest in order
		p.stopWaiting, _ = utils.ReadUint32(r)
	}

	for r.Len() > 0 {
		if stream, err := parseSegment(r); err != nil {
			return err
		} else {
			p.segments = append(p.segments, stream)
		}
	}

	return nil
}

func (p *Packet) encode() error {
	buf := new(bytes.Buffer)

	if p.ackFrame != nil {
		p.flag |= 0x80
	}

	if p.stopWaiting != 0 {
		p.flag |= 0x40
	}

	if len(p.segments) != 0 {
		p.flag |= 0x20
	}

	buf.WriteByte(p.flag)

	if p.ackFrame != nil {
		if err := p.ackFrame.Write(buf); err != nil {
			return err
		}
	}

	if p.flag != 0x80 {
		utils.WriteUint32(buf, p.PacketNumber)
	}

	if p.stopWaiting != 0 {
		utils.WriteUint32(buf, p.stopWaiting)
	}

	for _, f := range p.segments {
		if err := f.Write(buf); err != nil {
			return err
		}
	}

	p.D = make([]byte, buf.Len())
	copy(p.D, buf.Bytes())
	p.Length = uint32(len(p.D))

	return nil
}

// ParseAckFrame reads an ACK frame
func ParseAckFrame(r *bytes.Reader) (*AckFrame, error) {
	frame := &AckFrame{}

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
	frame.LargestAcked = uint32(largestAcked)

	delay, err := utils.ReadUfloat16(r)
	if err != nil {
		return nil, err
	}
	frame.DelayTime = time.Duration(delay) * time.Microsecond

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
		ackRange := AckRange{
			FirstPacketNumber: uint32(largestAcked-ackBlockLength) + 1,
			LastPacketNumber:  frame.LargestAcked,
		}
		frame.AckRanges = append(frame.AckRanges, ackRange)

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
				frame.AckRanges[len(frame.AckRanges)-1].FirstPacketNumber -= uint32(gap) + length
				frame.AckRanges[len(frame.AckRanges)-1].LastPacketNumber -= uint32(gap)
			} else {
				lastRangeComplete = false
				ackRange := AckRange{
					LastPacketNumber: frame.AckRanges[len(frame.AckRanges)-1].FirstPacketNumber - uint32(gap) - 1,
				}
				ackRange.FirstPacketNumber = ackRange.LastPacketNumber - length + 1
				frame.AckRanges = append(frame.AckRanges, ackRange)
			}

			if length > 0 {
				lastRangeComplete = true
			}

			inLongBlock = (ackBlockLength == 0)
		}

		// if the last range was not complete, FirstPacketNumber and LastPacketNumber make no sense
		// remove the range from frame.AckRanges
		if !lastRangeComplete {
			frame.AckRanges = frame.AckRanges[:len(frame.AckRanges)-1]
		}

		frame.LargestInOrder = frame.AckRanges[len(frame.AckRanges)-1].FirstPacketNumber
	} else {
		frame.LargestInOrder = uint32(largestAcked + 1 - ackBlockLength)
	}

	if !frame.validateAckRanges() {
		return nil, ErrInvalidAckRanges
	}

	var numTimestampByte byte
	numTimestampByte, err = r.ReadByte()
	if err != nil {
		return nil, err
	}
	numTimestamp := uint8(numTimestampByte)

	if numTimestamp > 0 {
		// Delta Largest acked
		_, err = r.ReadByte()
		if err != nil {
			return nil, err
		}
		// First Timestamp
		_, err = utils.ReadUint32(r)
		if err != nil {
			return nil, err
		}

		for i := 0; i < int(numTimestamp)-1; i++ {
			// Delta Largest acked
			_, err = r.ReadByte()
			if err != nil {
				return nil, err
			}

			// Time Since Previous Timestamp
			_, err = utils.ReadUint16(r)
			if err != nil {
				return nil, err
			}
		}
	}

	return frame, nil
}

// Write writes an ACK frame.
func (f *AckFrame) Write(b *bytes.Buffer) error {
	largestAckedLen := protocol.GetPacketNumberLength(protocol.PacketNumber(f.LargestAcked))

	typeByte := uint8(0x40)

	if largestAckedLen != protocol.PacketNumberLen1 {
		typeByte ^= (uint8(largestAckedLen / 2)) << 2
	}

	missingSequenceNumberDeltaLen := f.getMissingSequenceNumberDeltaLen()
	if missingSequenceNumberDeltaLen != protocol.PacketNumberLen1 {
		typeByte ^= (uint8(missingSequenceNumberDeltaLen / 2))
	}

	if f.HasMissingRanges() {
		typeByte |= 0x20
	}

	b.WriteByte(typeByte)

	switch largestAckedLen {
	case protocol.PacketNumberLen1:
		b.WriteByte(uint8(f.LargestAcked))
	case protocol.PacketNumberLen2:
		utils.WriteUint16(b, uint16(f.LargestAcked))
	case protocol.PacketNumberLen4:
		utils.WriteUint32(b, uint32(f.LargestAcked))
	case protocol.PacketNumberLen6:
		utils.WriteUint48(b, uint64(f.LargestAcked))
	}

	f.DelayTime = time.Now().Sub(f.PacketReceivedTime)
	utils.WriteUfloat16(b, uint64(f.DelayTime/time.Microsecond))

	var numRanges uint64
	var numRangesWritten uint64
	if f.HasMissingRanges() {
		numRanges = f.numWritableNackRanges()
		if numRanges > 0xFF {
			panic("AckFrame: Too many ACK ranges")
		}
		b.WriteByte(uint8(numRanges - 1))
	}

	var firstAckBlockLength uint32
	if !f.HasMissingRanges() {
		firstAckBlockLength = f.LargestAcked - f.LargestInOrder + 1
	} else {
		if f.LargestAcked != f.AckRanges[0].LastPacketNumber {
			return errInconsistentAckLargestAcked
		}
		if f.LargestInOrder != f.AckRanges[len(f.AckRanges)-1].FirstPacketNumber {
			fmt.Printf("largest in order: %d, sack: %v", f.LargestInOrder, f.AckRanges)
			return errInconsistentAckLowestAcked
		}
		firstAckBlockLength = f.LargestAcked - f.AckRanges[0].FirstPacketNumber + 1
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

	for i, ackRange := range f.AckRanges {
		if i == 0 {
			continue
		}

		length := ackRange.LastPacketNumber - ackRange.FirstPacketNumber + 1
		gap := f.AckRanges[i-1].FirstPacketNumber - ackRange.LastPacketNumber - 1

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
		fmt.Println("illegal ack: ", f)
		return errors.New("BUG: Inconsistent number of ACK ranges written")
	}

	b.WriteByte(0) // no timestamps

	return nil
}

func (f *AckFrame) HasMissingRanges() bool {
	if len(f.AckRanges) > 0 {
		return true
	}
	return false
}

// MinLength of a written frame
func (f *AckFrame) MinLength() (uint32, error) {
	var length uint32
	length = 1 + 2 + 1 // 1 TypeByte, 2 ACK delay time, 1 Num Timestamp
	length += uint32(protocol.GetPacketNumberLength(protocol.PacketNumber(f.LargestAcked)))

	missingSequenceNumberDeltaLen := uint32(f.getMissingSequenceNumberDeltaLen())

	if f.HasMissingRanges() {
		length += (1 + missingSequenceNumberDeltaLen) * uint32(f.numWritableNackRanges())
	} else {
		length += missingSequenceNumberDeltaLen
	}

	length += (1 + 2) * 0 /* TODO: num_timestamps */

	return length, nil
}

func (f *AckFrame) validateAckRanges() bool {
	if len(f.AckRanges) == 0 {
		return true
	}

	// if there are missing packets, there will always be at least 2 ACK ranges
	if len(f.AckRanges) == 1 {
		return false
	}

	if f.AckRanges[0].LastPacketNumber != f.LargestAcked {
		return false
	}

	// check the validity of every single ACK range
	for _, ackRange := range f.AckRanges {
		if ackRange.FirstPacketNumber > ackRange.LastPacketNumber {
			return false
		}
	}

	// check the consistency for ACK with multiple NACK ranges
	for i, ackRange := range f.AckRanges {
		if i == 0 {
			continue
		}
		lastAckRange := f.AckRanges[i-1]
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
func (f *AckFrame) numWritableNackRanges() uint64 {
	if len(f.AckRanges) == 0 {
		return 0
	}

	var numRanges uint64
	for i, ackRange := range f.AckRanges {
		if i == 0 {
			continue
		}

		lastAckRange := f.AckRanges[i-1]
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

func (f *AckFrame) getMissingSequenceNumberDeltaLen() protocol.PacketNumberLen {
	var maxRangeLength uint32

	if f.HasMissingRanges() {
		for _, ackRange := range f.AckRanges {
			rangeLength := ackRange.LastPacketNumber - ackRange.FirstPacketNumber + 1
			if rangeLength > maxRangeLength {
				maxRangeLength = rangeLength
			}
		}
	} else {
		maxRangeLength = f.LargestAcked - f.LargestInOrder + 1
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
