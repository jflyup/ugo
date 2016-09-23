package ugo

type segmentSender struct {
	c                   *Connection
	retransmissionQueue []*segment
}

func newSegmentSender(c *Connection) *segmentSender {
	return &segmentSender{
		c: c,
	}
}

func (f *segmentSender) AddSegmentForRetransmission(frame *segment) {
	f.retransmissionQueue = append(f.retransmissionQueue, frame)
}

func (f *segmentSender) PopSegments(maxLen uint64) []*segment {
	fs, currentLen := f.maybePopFramesForRetransmission(maxLen)
	return append(fs, f.maybePopNormalFrames(maxLen-currentLen)...)
}

func (f *segmentSender) maybePopFramesForRetransmission(maxLen uint64) (res []*segment, currentLen uint64) {
	for len(f.retransmissionQueue) > 0 {
		seg := f.retransmissionQueue[0]

		var frameHeaderLen uint64 = 4 // TODO
		if currentLen+frameHeaderLen > maxLen {
			break
		}

		currentLen += frameHeaderLen

		splitFrame := maybeSplitOffFrame(seg, maxLen-currentLen)
		if splitFrame != nil { // StreamFrame was split
			res = append(res, splitFrame)
			currentLen += splitFrame.DataLen()
			break
		}

		f.retransmissionQueue = f.retransmissionQueue[1:]
		res = append(res, seg)
		currentLen += seg.DataLen()
	}
	return
}

func (f *segmentSender) maybePopNormalFrames(maxBytes uint64) (res []*segment) {
	frame := &segment{}
	var currentLen uint64

	// not perfect, but thread-safe since writeOffset is only written when getting data
	frame.offset = f.c.writeOffset
	var frameHeaderBytes uint64 = 4 // TODO
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
		// do something?
		return
	}

	frame.data = data
	//f.flowControlManager.AddBytesSent(s.streamID, uint32(len(data)))

	res = append(res, frame)
	currentLen += frameHeaderBytes + frame.DataLen()

	return
}

// maybeSplitOffFrame removes the first n bytes and returns them as a separate frame. If n >= len(frame), nil is returned and nothing is modified.
func maybeSplitOffFrame(frame *segment, n uint64) *segment {
	if n >= frame.DataLen() {
		return nil
	}

	defer func() {
		frame.data = frame.data[n:]
		frame.offset += n
	}()

	return &segment{
		offset: frame.offset,
		data:   frame.data[:n],
	}
}
