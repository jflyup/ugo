package ugo

type segmentSender struct {
	c                   *Conn
	retransmissionQueue []*segment
}

func newSegmentSender(c *Conn) *segmentSender {
	return &segmentSender{
		c: c,
	}
}

func (f *segmentSender) AddSegmentForRetransmission(frame *segment) {
	f.retransmissionQueue = append(f.retransmissionQueue, frame)
}

func (f *segmentSender) PopSegments(maxLen uint64) []*segment {
	fs, currentLen := f.popSegmentsForRetransmission(maxLen)
	return append(fs, f.popNormalSegments(maxLen-currentLen)...)
}

func (f *segmentSender) popSegmentsForRetransmission(maxLen uint64) (res []*segment, currentLen uint64) {
	for len(f.retransmissionQueue) > 0 {
		seg := f.retransmissionQueue[0]

		var frameHeaderLen uint64 = 4 // TODO
		if currentLen+frameHeaderLen > maxLen {
			break
		}

		currentLen += frameHeaderLen

		splitSeg := maybeSplitOffFrame(seg, maxLen-currentLen)
		if splitSeg != nil { // segment was split
			res = append(res, splitSeg)
			currentLen += splitSeg.dataLen()
			break
		}

		f.retransmissionQueue = f.retransmissionQueue[1:]
		res = append(res, seg)
		currentLen += seg.dataLen()
	}
	return
}

func (f *segmentSender) popNormalSegments(maxBytes uint64) (res []*segment) {
	frame := &segment{}
	var currentLen uint64

	frame.offset = f.c.writeOffset
	var frameHeaderBytes uint64 = 4 // TODO
	if currentLen+frameHeaderBytes > maxBytes {
		return // theoretically, we could find another stream that fits, but this is quite unlikely, so we stop here
	}
	maxLen := maxBytes - currentLen - frameHeaderBytes

	if f.c.lenOfDataForWriting() != 0 {
		//sendWindowSize, _ := f.flowControlManager.SendWindowSize()
		//maxLen = utils.MinByteCount(maxLen, sendWindowSize)
	}

	//	if maxLen == 0 {
	//		continue
	//	}

	data := f.c.getDataForWriting(maxLen)
	if data == nil {
		// do something?
		return
	}

	frame.data = data
	//f.flowControlManager.AddBytesSent(s.streamID, uint32(len(data)))

	res = append(res, frame)
	currentLen += frameHeaderBytes + frame.dataLen()

	return
}

// maybeSplitOffFrame removes the first n bytes and returns them as a separate frame.
// If n >= len(frame), nil is returned and nothing is modified.
func maybeSplitOffFrame(s *segment, n uint64) *segment {
	if n >= s.dataLen() {
		return nil
	}

	defer func() {
		s.data = s.data[n:]
		s.offset += n
	}()

	return &segment{
		offset: s.offset,
		data:   s.data[:n],
	}
}
