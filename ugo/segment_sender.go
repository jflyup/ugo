package ugo

import (
	"log"
)

type segmentSender struct {
	c                   *connection
	retransmissionQueue []*segment
}

func newSegmentSender(c *connection) *segmentSender {
	return &segmentSender{
		c: c,
	}
}

func (f *segmentSender) AddSegmentForRetransmission(frame *segment) {
	f.retransmissionQueue = append(f.retransmissionQueue, frame)
}

func (f *segmentSender) PopSegments(maxLen uint32) []*segment {
	fs, currentLen := f.maybePopFramesForRetransmission(maxLen)
	return append(fs, f.maybePopNormalFrames(maxLen-currentLen)...)
}

func (f *segmentSender) maybePopFramesForRetransmission(maxLen uint32) (res []*segment, currentLen uint32) {
	for len(f.retransmissionQueue) > 0 {
		frame := f.retransmissionQueue[0]

		log.Printf("resending segment, length: %d", frame.DataLen())
		var frameHeaderLen uint32 = 4 // TODO
		if currentLen+frameHeaderLen > maxLen {
			break
		}

		currentLen += frameHeaderLen

		splitFrame := maybeSplitOffFrame(frame, maxLen-currentLen)
		if splitFrame != nil { // StreamFrame was split
			res = append(res, splitFrame)
			currentLen += splitFrame.DataLen()
			break
		}

		f.retransmissionQueue = f.retransmissionQueue[1:]
		res = append(res, frame)
		currentLen += frame.DataLen()
	}
	return
}

func (f *segmentSender) maybePopNormalFrames(maxBytes uint32) (res []*segment) {
	frame := &segment{}
	var currentLen uint32

	// not perfect, but thread-safe since writeOffset is only written when getting data
	frame.Offset = f.c.writeOffset
	var frameHeaderBytes uint32 = 4 // TODO
	if currentLen+frameHeaderBytes > maxBytes {
		return // theoretically, we could find another stream that fits, but this is quite unlikely, so we stop here
	}
	maxLen := maxBytes - currentLen - frameHeaderBytes

	if f.c.lenOfDataForWriting() != 0 {
		//sendWindowSize, _ := f.flowControlManager.SendWindowSize(s.streamID)
		//maxLen = utils.MinByteCount(maxLen, sendWindowSize)
	}

	//	if maxLen == 0 {
	//		continue
	//	}

	data := f.c.getDataForWriting(maxLen)
	if data == nil {
		//		if f.s.shouldSendFin() {
		//			frame.FinBit = true
		//			f.s.sentFin()
		//			res = append(res, frame)
		//			currentLen += frameHeaderBytes + frame.DataLen()
		//			frame = &StreamFrame{}
		//		}
		return
	}

	frame.Data = data
	//f.flowControlManager.AddBytesSent(s.streamID, uint32(len(data)))

	res = append(res, frame)
	currentLen += frameHeaderBytes + frame.DataLen()

	return
}

// maybeSplitOffFrame removes the first n bytes and returns them as a separate frame. If n >= len(frame), nil is returned and nothing is modified.
func maybeSplitOffFrame(frame *segment, n uint32) *segment {
	if n >= frame.DataLen() {
		return nil
	}

	defer func() {
		frame.Data = frame.Data[n:]
		frame.Offset += n
	}()

	return &segment{
		FinBit: false,
		Offset: frame.Offset,
		Data:   frame.Data[:n],
	}
}
