package ugo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"./protocol"

	"./utils"
)

var (
	errTimeout = errors.New("operation timeout")
)

type connection struct {
	conn         net.PacketConn
	connectionID protocol.ConnectionID

	addr      net.Addr
	localAddr net.Addr

	sentPacketHandler     *sentPacketHandler
	receivedPacketHandler *receivedPacketHandler
	segmentSender         *segmentSender

	receivedPackets  chan []byte
	sendingScheduled chan struct{}

	closed int32 // atomic bool
	eof    int32 // really a bool

	delayedAckOriginTime time.Time

	lastRcvdPacketNumber uint32

	lastNetworkActivityTime time.Time

	timer           *time.Timer
	currentDeadline time.Time
	timerRead       bool
	fec             *FEC
	crypt           StreamCrypto
	err             error
	mutex           sync.Mutex
	wmu             sync.Mutex
	segmentQueue    *segmentSorter
	chRead          chan struct{}
	chWrite         chan struct{}

	dataForWriting []byte
	finSent        bool
	closeChan      chan error

	readTimeout  time.Time // read deadline
	writeTimeout time.Time // write deadline

	readPosInFrame uint32
	writeOffset    uint32
	readOffset     uint32
	closeCallback  func()

	lastPacketNumber uint32
}

func newConnection(pc net.PacketConn, addr net.Addr, connectionID protocol.ConnectionID, crypt StreamCrypto, fec *FEC, close func()) *connection {
	c := &connection{
		connectionID:  connectionID,
		conn:          pc,
		addr:          addr,
		localAddr:     pc.LocalAddr(),
		closeCallback: close,

		sentPacketHandler:     newSentPacketHandler(),
		receivedPacketHandler: newReceivedPacketHandler(),

		segmentQueue: newStreamFrameSorter(), // used for incoming segments reordering

		receivedPackets:  make(chan []byte, 1024),
		sendingScheduled: make(chan struct{}, 1),
		chRead:           make(chan struct{}, 1),
		chWrite:          make(chan struct{}, 1),

		closeChan: make(chan error, 1),
		timer:     time.NewTimer(0),
		lastNetworkActivityTime: time.Now(),
		crypt: crypt,
		fec:   fec,
	}

	c.segmentSender = newSegmentSender(c) // used for outcomming segments

	return c
}

// run the session main loop
func (c *connection) run() {
	for {
		// Close immediately if requested
		select {
		case <-c.closeChan:
			return
		default:
		}

		c.resetTimer()

		var err error
		select {
		//		case errForConnClose := <-s.closeChan:
		//			//			if errForConnClose != nil {
		//			//				s.sendConnectionClose(errForConnClose)
		//			//			}
		//			return
		case <-c.timer.C:
			c.timerRead = true
			// We do all the interesting stuff after the switch statement, so
			// nothing to see here.
		case <-c.sendingScheduled:
			// We do all the interesting stuff after the switch statement, so
			// nothing to see here.
		case p := <-c.receivedPackets:
			err = c.handlePacket(p)

			if c.delayedAckOriginTime.IsZero() {
				c.delayedAckOriginTime = time.Now()
			}
		}

		if err != nil {
			log.Println("handle error", err)
			c.Close()
			return
		}

		if err := c.sendPacket(); err != nil {
			log.Println("send error", err)
			c.Close()
			return
		}

		if time.Now().Sub(c.lastNetworkActivityTime) >= InitialIdleConnectionStateLifetime {
			c.CloseWithError(errors.New("No recent network activity."))
		}
	}
}

// Implementation of the Conn interface.
func (s *connection) Read(p []byte) (int, error) {
	if atomic.LoadInt32(&s.eof) != 0 {
		return 0, io.EOF
	}

	bytesRead := 0
	for bytesRead < len(p) {
		s.mutex.Lock()
		frame := s.segmentQueue.Head()

		if frame == nil && bytesRead > 0 {
			s.mutex.Unlock()
			return bytesRead, s.err
		}

		if !s.readTimeout.IsZero() {
			if time.Now().After(s.readTimeout) { // timeout
				s.mutex.Unlock()
				return bytesRead, errTimeout
			}
		}

		for {
			// Stop waiting on errors
			if s.err != nil {
				break
			}
			if frame != nil {
				// Pop and continue if the frame doesn't have any new data
				if frame.Offset+frame.DataLen() <= s.readOffset {
					s.segmentQueue.Pop()
					frame = s.segmentQueue.Head()

					continue
				}
				// If the frame's offset is <= our current read pos, and we didn't
				// go into the previous if, we can read data from the frame.
				if frame.Offset <= s.readOffset {
					// Set our read position in the frame properly
					s.readPosInFrame = s.readOffset - frame.Offset
					break
				}
			}

			s.mutex.Unlock()
			var timeout <-chan time.Time
			if !s.readTimeout.IsZero() {
				delay := s.readTimeout.Sub(time.Now())
				timeout = time.After(delay)
			}

			// wait for data or timeout
			select {
			case <-s.chRead:
				log.Println("recv read event")
				s.mutex.Lock()
				frame = s.segmentQueue.Head()
			case <-timeout:
				return bytesRead, errTimeout
			case <-s.closeChan:
				return 0, io.ErrClosedPipe
			}
		}
		s.mutex.Unlock()

		if frame == nil {
			atomic.StoreInt32(&s.eof, 1)
			// We have an err and no data, return the error
			return bytesRead, s.err
		}

		m := uint32(utils.Min(len(p)-bytesRead, int(frame.DataLen())) - int(s.readPosInFrame))
		copy(p[bytesRead:], frame.Data[s.readPosInFrame:])

		s.readPosInFrame += m
		bytesRead += int(m)
		s.readOffset += m

		//		s.flowControlManager.AddBytesRead(s.streamID, uint32(m))
		//		s.onData() // so that a possible WINDOW_UPDATE is sent
		if s.readPosInFrame >= frame.DataLen() {
			fin := frame.FinBit
			s.mutex.Lock()
			s.segmentQueue.Pop()
			s.mutex.Unlock()
			if fin {
				atomic.StoreInt32(&s.eof, 1)
				return int(bytesRead), io.EOF
			}
		}
	}

	return bytesRead, nil
}

func (s *connection) Write(p []byte) (int, error) {
	if atomic.LoadInt32(&s.closed) != 0 {
		return 0, io.ErrClosedPipe
	}
	s.wmu.Lock()

	s.dataForWriting = make([]byte, len(p))
	copy(s.dataForWriting, p)

	s.scheduleSending()

	var timeout <-chan time.Time
	if !s.writeTimeout.IsZero() {
		delay := s.writeTimeout.Sub(time.Now())
		timeout = time.After(delay)
	}

	s.wmu.Unlock()
	for s.dataForWriting != nil && s.err == nil {
		select {
		case <-s.chWrite:
			break
		case <-timeout:
			return 0, errTimeout
		case <-s.closeChan:
			return 0, io.ErrClosedPipe
		}
	}

	if s.err != nil {
		return 0, s.err
	}
	return len(p), nil
}

func (c *connection) Close() error {
	// Only close once
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return nil
	}
	c.sendConnectionClose(nil)
	close(c.closeChan)
	atomic.StoreInt32(&c.closed, 1)
	c.closeCallback()
	return nil
	//return s.closeImpl(nil, true)
}

func (c *connection) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *connection) RemoteAddr() net.Addr {
	return c.addr
}

func (c *connection) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

func (c *connection) SetReadDeadline(t time.Time) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.readTimeout = t
	return nil
}

func (c *connection) SetWriteDeadline(t time.Time) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.writeTimeout = t
	return nil
}

func (c *connection) resetTimer() {
	nextDeadline := c.lastNetworkActivityTime.Add(InitialIdleConnectionStateLifetime)

	if !c.delayedAckOriginTime.IsZero() {
		nextDeadline = utils.MinTime(nextDeadline, c.delayedAckOriginTime.Add(protocol.AckSendDelay))
	}
	if rtoTime := c.sentPacketHandler.TimeOfFirstRTO(); !rtoTime.IsZero() {
		nextDeadline = utils.MinTime(nextDeadline, rtoTime)
	}

	if nextDeadline.Equal(c.currentDeadline) {
		// No need to reset the timer
		return
	}

	// We need to drain the timer if the value from its channel was not read yet.
	// See https://groups.google.com/forum/#!topic/golang-dev/c9UUfASVPoU
	if !c.timer.Stop() && !c.timerRead {
		<-c.timer.C
	}
	c.timer.Reset(nextDeadline.Sub(time.Now()))

	c.timerRead = false
	c.currentDeadline = nextDeadline
}

func (c *connection) handlePacket(data []byte) error {
	c.lastNetworkActivityTime = time.Now()

	c.crypt.Decrypt(data, data)
	// TODO check data integrity
	if c.fec != nil {
		f := c.fec.decode(data)
		if f.Flag() == typeData || f.Flag() == typeFEC {
			if recovered := c.fec.input(f); recovered != nil {
				for k := range recovered {
					fmt.Println("recovered:", binary.LittleEndian.Uint32(recovered[k]))
				}
			}
		}

		if f.Flag() == typeData {
			data = data[fecHeaderSize:] // remove fec packet header
		}
	}

	// TODO reference count buffer
	p := &Packet{
		D: make([]byte, len(data)),
	}

	copy(p.D[0:], data)
	p.Length = uint32(len(p.D))
	// decode
	if err := p.decode(); err != nil {
		log.Println("recv invalid data:", p.D)
		return err
	}

	log.Printf("recv packet %d, length %d", p.PacketNumber, p.Length)
	if p.flag == 128 {
		log.Println("recv ack", p.ackFrame, time.Now().String())
	}

	if p.flag&0x01 != 0 {
		log.Println("recv weird data:", p.D)
		// TODO should close the connection
		return nil
	}

	if p.flag == 0x10 {
		log.Println("recv close:", p.PacketNumber)
		// peer has already gone
		// no TIME_WAIT CLOSE_WAIT like TCP
		c.Close()
		return nil
	}

	if p.PacketNumber != 0 {
		if err := c.receivedPacketHandler.ReceivedPacket(p.PacketNumber); err != nil {
			log.Printf("recv packets error %v, rcvhistory %v", err, c.receivedPacketHandler.packetHistory.ranges.Len())
			return err
		}
	}

	if p.ackFrame != nil {
		if err := c.handleAckFrame(p.ackFrame, p.PacketNumber); err != nil {
			return err
		}
	}

	for _, frame := range p.segments {
		if err := c.handleSegment(frame); err != nil {
			return err
		}
	}

	if p.stopWaiting != 0 {
		log.Printf("recv stop waiting %d from %s", p.stopWaiting, c.addr.String())
		c.receivedPacketHandler.ReceivedStopWaiting(p.stopWaiting)
	}

	return nil
}

func (s *connection) handleSegment(frame *segment) error {
	s.mutex.Lock()

	err := s.segmentQueue.Push(frame)
	if err != nil && err != errDuplicateStreamData {
		s.mutex.Unlock()
		return err
	}
	s.mutex.Unlock()

	s.chRead <- struct{}{}

	return nil
}

//func (s *session) handleWindowUpdateFrame(frame *frames.WindowUpdateFrame) error {
//	s.streamsMutex.RLock()
//	defer s.streamsMutex.RUnlock()
//	if frame.StreamID != 0 {
//		stream, ok := s.streams[frame.StreamID]
//		if ok && stream == nil {
//			return errWindowUpdateOnClosedStream
//		}

//		// open new stream when receiving a WindowUpdate for a non-existing stream
//		// this can occur if the client immediately sends a WindowUpdate for a newly opened stream, and packet reordering occurs such that the packet opening the new stream arrives after the WindowUpdate
//		if !ok {
//			s.newStreamImpl(frame.StreamID)
//		}
//	}
//	_, err := s.flowControlManager.UpdateWindow(frame.StreamID, frame.ByteOffset)
//	return err
//}

func (s *connection) handleAckFrame(frame *AckFrame, packetNum uint32) error {
	if err := s.sentPacketHandler.ReceivedAck(frame, packetNum); err != nil {
		return err
	}
	return nil
}

// Close the connection. If err is nil it will be set to qerr.PeerGoingAway.
func (s *connection) CloseWithError(e error) error {
	return s.closeImpl(e, false)
}

func (c *connection) closeImpl(e error, remoteClose bool) error {
	// Only close once
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return nil
	}

	//c.close() // TODO

	if remoteClose {
		// If this is a remote close we don't need to send a CONNECTION_CLOSE
		//s.closeChan <- nil
		return nil
	}

	//s.closeChan <- quicErr
	return nil
}

func (s *connection) sendPacket() error {
	// Repeatedly try sending until we don't have any more data, or run out of the congestion window
	if atomic.LoadInt32(&s.closed) != 0 {
		return nil
	}
	// TODO split send and recv in 2 goroutine
	for {
		err := s.sentPacketHandler.CheckForError()
		if err != nil {
			return err
		}

		if !s.sentPacketHandler.CongestionAllowsSending() {
			log.Printf("congestion not allow, size: %d, bytes outstanding: %d", s.sentPacketHandler.congestion.GetCongestionWindow(), s.sentPacketHandler.BytesInFlight())
			return nil
		}

		retransmitPacket := s.sentPacketHandler.DequeuePacketForRetransmission()

		if retransmitPacket != nil {
			// TODO resend flag

			// don't resend packet which only contains ack info
			for _, streamFrame := range retransmitPacket.segments {
				log.Println("retransmit segment", streamFrame.Offset)
				s.segmentSender.AddSegmentForRetransmission(streamFrame)
			}
		}

		ack, err := s.receivedPacketHandler.GetAckFrame(false)
		if err != nil {
			return err
		}

		stopWait := s.sentPacketHandler.GetStopWaitingFrame()

		if stopWait != 0 {
			log.Println("send stop waiting: ", stopWait)
		}

		// Check whether we are allowed to send a packet containing only an ACK
		//maySendOnlyAck := time.Now().Sub(s.delayedAckOriginTime) > protocol.AckSendDelay

		// get data
		frames := s.segmentSender.PopSegments(protocol.MaxPacketSize - 40) // TODO

		if ack == nil && len(frames) == 0 && stopWait == 0 {
			return nil
		}

		// Pop the ACK frame now that we are sure we're gonna send it
		if ack != nil {
			_, err = s.receivedPacketHandler.GetAckFrame(true)
			if err != nil {
				return err
			}
		}

		//		if len(frames) == 0 { // send ack only
		//			if !maySendOnlyAck {
		//				return nil
		//			}
		//		}

		if len(frames) != 0 || stopWait != 0 { // ack only
			s.lastPacketNumber++
		}

		pkt := &Packet{
			PacketNumber: s.lastPacketNumber,
			ackFrame:     ack,
			segments:     frames,
			stopWaiting:  stopWait,
		}

		if err := pkt.encode(); err != nil {
			log.Println("encode error, packet:", pkt)
			return err
		}

		if pkt.flag == 0x80 {
			pkt.PacketNumber = 0
		}

		if ack != nil {
			log.Printf("send ack, pkt num:%d, ack %v, time %s", pkt.PacketNumber, ack, time.Now().String())
		}

		log.Printf("sending packet %d to %s\n, data length: %d", pkt.PacketNumber, s.addr, len(pkt.D))
		err = s.sentPacketHandler.SentPacket(pkt)
		if err != nil {
			return err
		}

		s.delayedAckOriginTime = time.Time{}

		if pkt.flag&0x01 != 0 {
			log.Println("send invalild data:", pkt.D)
		}
		s.crypt.Encrypt(pkt.D, pkt.D)
		_, err = s.conn.WriteTo(pkt.D, s.addr)
		if err != nil {
			return err
		}

	}
}

//func (s *session) sendEncryptedData(c net.PacketConn, addr net.Addr) {
//	var fecGroup [][]byte
//	fecCnt := 0
//	var fecMaxSize int

//	if s.fec != nil {
//		fecGroup = make([][]byte, 13)
//		for k := range fecGroup {
//			fecGroup[k] = make([]byte, maxPacketSize)
//		}
//	}
//	for {
//		packet := <-s.chPlainPacket
//		oriData := packet.D

//		var ecc [][]byte
//		if s.fec != nil {
//			s.fec.markData(oriData)

//			copy(fecGroup[fecCnt], oriData)
//			fecCnt++

//			if len(oriData) > fecMaxSize {
//				fecMaxSize = len(oriData)
//			}

//			if fecCnt == s.fec.dataShards {
//				ecc = s.fec.calcECC(fecGroup, fecHeaderSize, fecMaxSize)
//				for k := range ecc {
//					s.fec.markFEC(ecc[k][0:])
//					ecc[k] = ecc[k][:fecMaxSize]
//				}
//				fecCnt = 0
//				fecMaxSize = 0
//			}
//		}

//		// Encrypt
//		s.crypt.Encrypt(oriData, oriData)
//		if ecc != nil {
//			for k := range ecc {
//				s.crypt.Encrypt(ecc[k], ecc[k])
//			}
//		}

//		fmt.Printf("send to %v %d bytes\n", addr, len(oriData))
//		c.WriteTo(oriData, addr)
//		packet.Free()
//		if ecc != nil {
//			for k := range ecc {
//				c.WriteTo(ecc[k], addr)
//			}
//		}
//	}
//}

func (c *connection) sendConnectionClose(err error) {
	c.lastPacketNumber++
	pkt := &Packet{
		flag:         0x10,
		PacketNumber: c.lastPacketNumber,
	}

	pkt.encode()
	c.sentPacketHandler.SentPacket(pkt)
	c.crypt.Encrypt(pkt.D, pkt.D)

	c.conn.WriteTo(pkt.D, c.addr)
}

// scheduleSending signals that we have data for sending
func (c *connection) scheduleSending() {
	select {
	case c.sendingScheduled <- struct{}{}:
	default:
	}
}

func (c *connection) lenOfDataForWriting() uint32 {
	c.wmu.Lock()
	l := uint32(len(c.dataForWriting))
	c.wmu.Unlock()
	return l
}

func (c *connection) getDataForWriting(maxBytes uint32) []byte {
	c.wmu.Lock()
	if c.dataForWriting == nil {
		c.wmu.Unlock()
		return nil
	}
	var ret []byte
	if uint32(len(c.dataForWriting)) > maxBytes {
		ret = c.dataForWriting[:maxBytes]
		c.dataForWriting = c.dataForWriting[maxBytes:]
	} else {
		ret = c.dataForWriting
		c.dataForWriting = nil
	}
	c.writeOffset += uint32(len(ret))
	c.wmu.Unlock()
	c.chWrite <- struct{}{}

	return ret
}

func (c *connection) shouldSendFin() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return atomic.LoadInt32(&c.closed) != 0 && !c.finSent && c.err == nil && c.dataForWriting == nil
}

func (c *connection) sentFin() {
	c.mutex.Lock()
	c.finSent = true
	c.mutex.Unlock()
}
