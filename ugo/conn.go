package ugo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jflyup/ugo/ugo/protocol"

	"github.com/jflyup/ugo/ugo/utils"
)

var (
	errTimeout = errors.New("operation timeout")
)

type connection struct {
	conn         net.PacketConn
	connectionID protocol.ConnectionID

	addr      net.Addr
	localAddr net.Addr

	packetSender   *packetSender
	packetReceiver *packetReceiver
	segmentSender  *segmentSender

	receivedPackets  chan []byte
	sendingScheduled chan struct{}

	closed int32 // atomic bool
	eof    int32 // really a bool

	ackNoDelay    bool
	originAckTime time.Time

	lastRcvdPacketNumber uint32

	lastNetworkActivityTime time.Time

	timer           *time.Timer
	currentDeadline time.Time
	timerRead       bool
	fec             *FEC
	crypt           streamCrypto
	err             error
	mutex           sync.Mutex
	wmu             sync.Mutex
	segmentQueue    *segmentSorter
	chRead          chan struct{}
	chWrite         chan struct{}

	dataForWriting []byte
	finSent        bool
	closeChan      chan error
	allSent        *sync.Cond

	readTimeout  time.Time // read deadline
	writeTimeout time.Time // write deadline

	readPosInFrame uint32
	writeOffset    uint32
	readOffset     uint32
	closeCallback  func()

	lastPacketNumber uint32
}

func newConnection(pc net.PacketConn, addr net.Addr, connectionID protocol.ConnectionID, crypt streamCrypto, fec *FEC, close func()) *connection {
	c := &connection{
		connectionID:  connectionID,
		conn:          pc,
		addr:          addr,
		localAddr:     pc.LocalAddr(),
		closeCallback: close,

		packetSender:   newPacketSender(),
		packetReceiver: newPacketReceiver(),

		segmentQueue: newSegmentSorter(), // used for incoming segments reordering

		receivedPackets:  make(chan []byte, 1024),
		sendingScheduled: make(chan struct{}, 1),
		chRead:           make(chan struct{}, 1),
		chWrite:          make(chan struct{}, 1),
		allSent:          &sync.Cond{L: &sync.Mutex{}},

		closeChan: make(chan error, 1), // use Close(closeChan) to broadcast
		timer:     time.NewTimer(0),
		lastNetworkActivityTime: time.Now(),
		crypt: crypt,
		fec:   fec,
	}

	c.segmentSender = newSegmentSender(c) // used for outcomming segments
	//c.SetACKNoDelay(true)
	return c
}

func (c *connection) run() {
	for {
		// Close immediately if requested
		select {
		case <-c.closeChan:
			log.Printf("close connection with %s", c.addr.String())
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
		case <-c.sendingScheduled:
		case p := <-c.receivedPackets:
			err = c.handlePacket(p)

			if c.originAckTime.IsZero() {
				c.originAckTime = time.Now()
			}
		}

		if err != nil {
			log.Println("handle error", err)
			c.Close()
			return
		}
		// sendPacket may take a long time if continuous Write()
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

// implementation of the net.Conn interface.
func (c *connection) Read(p []byte) (int, error) {
	if atomic.LoadInt32(&c.eof) != 0 {
		return 0, io.EOF
	}

	bytesRead := 0
	for bytesRead < len(p) {
		c.mutex.Lock()
		frame := c.segmentQueue.Head()

		if frame == nil && bytesRead > 0 {
			c.mutex.Unlock()
			return bytesRead, c.err
		}

		if !c.readTimeout.IsZero() {
			if time.Now().After(c.readTimeout) { // timeout
				c.mutex.Unlock()
				return bytesRead, errTimeout
			}
		}

		for {
			// Stop waiting on errors
			if c.err != nil {
				break
			}
			if frame != nil {
				// Pop and continue if the frame doesn't have any new data
				if frame.Offset+frame.DataLen() <= c.readOffset {
					c.segmentQueue.Pop()
					frame = c.segmentQueue.Head()

					continue
				}
				// If the frame's offset is <= our current read pos, and we didn't
				// go into the previous if, we can read data from the frame.
				if frame.Offset <= c.readOffset {
					// Set our read position in the frame properly
					c.readPosInFrame = c.readOffset - frame.Offset
					break
				}
			}

			c.mutex.Unlock()
			var timeout <-chan time.Time
			if !c.readTimeout.IsZero() {
				delay := c.readTimeout.Sub(time.Now())
				timeout = time.After(delay)
			}

			// wait for data or timeout
			select {
			case <-c.chRead:
				c.mutex.Lock()
				frame = c.segmentQueue.Head()
			case <-timeout:
				return bytesRead, errTimeout
			case <-c.closeChan:
				return 0, io.ErrClosedPipe
			}
		}
		c.mutex.Unlock()

		if frame == nil {
			atomic.StoreInt32(&c.eof, 1)
			// We have an err and no data, return the error
			return bytesRead, c.err
		}

		m := uint32(utils.Min(len(p)-bytesRead, int(frame.DataLen())) - int(c.readPosInFrame))
		copy(p[bytesRead:], frame.data[c.readPosInFrame:])

		c.readPosInFrame += m
		bytesRead += int(m)
		c.readOffset += m

		//		s.flowControlManager.AddBytesRead(s.streamID, uint32(m))
		//		s.onData() // so that a possible WINDOW_UPDATE is sent
		if c.readPosInFrame >= frame.DataLen() {
			c.mutex.Lock()
			c.segmentQueue.Pop()
			c.mutex.Unlock()
		}
	}

	return bytesRead, nil
}

func (c *connection) Write(p []byte) (int, error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		return 0, io.ErrClosedPipe
	}
	c.wmu.Lock()

	c.dataForWriting = make([]byte, len(p))
	copy(c.dataForWriting, p)
	c.wmu.Unlock()
	c.scheduleSending()

	var timeout <-chan time.Time
	if !c.writeTimeout.IsZero() {
		delay := c.writeTimeout.Sub(time.Now())
		timeout = time.After(delay)
	}

	for {
		select {
		case <-c.chWrite:
			return len(p), nil
		case <-timeout:
			return 0, errTimeout
		case <-c.closeChan:
			return 0, io.ErrClosedPipe
		}
	}

}

// TODO 2 ways(reset and graceful shutdown) to terminate a connection
func (c *connection) Close() error {
	_, file, no, ok := runtime.Caller(1)
	if ok {
		log.Printf("called from %s#%d\n", file, no)
	}
	// Only close once
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return nil
	}

	// prevent any more Write()
	atomic.StoreInt32(&c.closed, 1)

	// wait until all queued messages for the connection have been successfully sent(sent and acked) or
	// the timeout has been reached. kind of SO_LINGER option in TCP.
	c.allSent.L.Lock()
	for len(c.packetSender.packetHistory) != 0 {
		c.allSent.Wait()
	}
	c.allSent.L.Unlock()

	c.sendConnectionClose(nil)
	close(c.closeChan)

	c.closeCallback()
	return nil
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

func (c *connection) SetACKNoDelay(nodelay bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.ackNoDelay = nodelay
}

// TODO timer queue
func (c *connection) resetTimer() {
	nextDeadline := c.lastNetworkActivityTime.Add(InitialIdleConnectionStateLifetime)

	if !c.originAckTime.IsZero() {
		nextDeadline = utils.MinTime(nextDeadline, c.originAckTime.Add(protocol.AckSendDelay))
	}
	if rtoTime := c.packetSender.TimeOfFirstRTO(); !rtoTime.IsZero() {
		nextDeadline = utils.MinTime(nextDeadline, rtoTime)
	}

	if nextDeadline.Equal(c.currentDeadline) {
		// No need to reset the timer
		return
	}

	// drain the timer if the value from its channel was not read yet.
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
		// TODO
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
	p := &ugoPacket{
		rawData: make([]byte, len(data)),
	}

	copy(p.rawData[0:], data)
	p.Length = uint32(len(p.rawData))
	// decode
	if err := p.decode(); err != nil {
		log.Printf("err: %v, recv invalid data: %v from %s", err, p.rawData, c.addr.String())
		return err
	}

	log.Printf("%s recv packet %d, length %d", c.localAddr.String(), p.packetNumber, p.Length)

	if p.flag&0x01 != 0 {
		log.Println("recv weird data:", p.rawData)
		// TODO should close the connection
		return nil
	}

	if p.flag == 0x10 {
		log.Println("recv close:", p.packetNumber)
		// peer already gone, close immediately
		// no TIME_WAIT CLOSE_WAIT like TCP for now
		close(c.closeChan)
		c.closeCallback()
		return nil
	}

	if p.packetNumber != 0 {
		if err := c.packetReceiver.ReceivedPacket(p.packetNumber); err != nil {
			log.Printf("recv packets error %v, recv history %v", err, c.packetReceiver.packetHistory.ranges.Len())
			return err
		}
	}

	if p.sack != nil {
		log.Println("recv ack", p.sack)
		if err := c.handleSack(p.sack, p.packetNumber); err != nil {
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
		c.packetReceiver.ReceivedStopWaiting(p.stopWaiting)
	}

	return nil
}

func (c *connection) handleSegment(s *segment) error {
	c.mutex.Lock()

	err := c.segmentQueue.Push(s)
	if err != nil && err != errDuplicateStreamData {
		c.mutex.Unlock()
		return err
	}
	c.mutex.Unlock()

	// nonblocking
	select {
	case c.chRead <- struct{}{}:
	default:
	}

	return nil
}

func (c *connection) handleSack(ack *sack, packetNum uint32) error {
	c.allSent.L.Lock()
	defer c.allSent.L.Unlock()

	if err := c.packetSender.ReceivedAck(ack, packetNum); err != nil {
		return err
	}
	if len(c.packetSender.packetHistory) == 0 {
		c.allSent.Signal()
	}

	return nil
}

// TODO reset connection if any error
func (c *connection) CloseWithError(e error) error {
	return c.closeImpl(e, false)
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

	return nil
}

func (c *connection) sendPacket() error {
	// Repeatedly try sending until no more data remained,
	// or run out of the congestion window
	if atomic.LoadInt32(&c.closed) != 0 {
		return nil
	}

	// TODO split send and recv in 2 goroutine
	for {
		err := c.packetSender.CheckForError()
		if err != nil {
			return err
		}

		if !c.packetSender.CongestionAllowsSending() {
			log.Printf("congestion not allow, size: %d, bytes outstanding: %d", c.packetSender.congestion.GetCongestionWindow(), c.packetSender.BytesInFlight())
			return nil
		}

		retransmitPacket := c.packetSender.DequeuePacketForRetransmission()

		if retransmitPacket != nil {
			// if retransmitted packet contains control message
			if retransmitPacket.flag&0x80 == 0x80 {
				c.packetReceiver.stateChanged = true
			}
			if retransmitPacket.flag&0x40 == 0x40 {
				c.packetSender.stopWaitingManager.state = true
			}

			for _, streamFrame := range retransmitPacket.segments {
				log.Println("retransmit segment", streamFrame.Offset)
				c.segmentSender.AddSegmentForRetransmission(streamFrame)
			}
		}

		// TODO function pack()
		ack, err := c.packetReceiver.GetAckFrame(false)
		if err != nil {
			return err
		}

		stopWait := c.packetSender.GetStopWaitingFrame()

		// get data
		frames := c.segmentSender.PopSegments(protocol.MaxPacketSize - 40) // TODO

		if ack == nil && len(frames) == 0 && stopWait == 0 {
			return nil
		}

		// Check whether we are allowed to send a packet containing only an ACK
		onlyAck := time.Now().Sub(c.originAckTime) > protocol.AckSendDelay || c.ackNoDelay

		if len(frames) == 0 && stopWait == 0 {
			if !onlyAck {
				return nil
			}
		}

		// Pop the ACK frame now that we are sure we're gonna send it
		if ack != nil {
			_, err = c.packetReceiver.GetAckFrame(true)
			if err != nil {
				return err
			}
		}

		if len(frames) != 0 || stopWait != 0 {
			c.lastPacketNumber++
		}

		pkt := &ugoPacket{
			packetNumber: c.lastPacketNumber,
			sack:         ack,
			segments:     frames,
			stopWaiting:  stopWait,
		}

		if err := pkt.encode(); err != nil {
			log.Println("encode error, packet:", pkt)
			return err
		}

		if pkt.flag == 0x80 {
			pkt.packetNumber = 0
		}

		log.Printf("%s sending packet %d to %s\n, data length: %d", c.localAddr.String(), pkt.packetNumber, c.addr, len(pkt.rawData))
		if pkt.packetNumber != 0 {
			c.allSent.L.Lock()
			err = c.packetSender.SentPacket(pkt)
			if err != nil {
				c.allSent.L.Unlock()
				return err
			}
			if len(c.packetSender.packetHistory) == 0 {
				c.allSent.Signal()
			}
			c.allSent.L.Unlock()
		}

		c.originAckTime = time.Time{}

		if pkt.flag&0x01 != 0 {
			log.Println("send invalid data:", pkt.rawData)
		}

		c.crypt.Encrypt(pkt.rawData, pkt.rawData)

		_, err = c.conn.WriteTo(pkt.rawData, c.addr)
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
//		if ecc != nil {
//			for k := range ecc {
//				c.WriteTo(ecc[k], addr)
//			}
//		}
//	}
//}

func (c *connection) sendConnectionClose(err error) {
	//c.lastPacketNumber++
	pkt := &ugoPacket{
		flag:         0x10,
		packetNumber: 0,
	}

	pkt.encode()
	//c.sentPacketHandler.SentPacket(pkt)
	log.Printf("%s send close to %s", c.localAddr.String(), c.RemoteAddr().String())
	c.crypt.Encrypt(pkt.rawData, pkt.rawData)
	c.conn.WriteTo(pkt.rawData, c.addr)
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
		select {
		case c.chWrite <- struct{}{}:
		default:
		}
	}
	c.writeOffset += uint32(len(ret))
	c.wmu.Unlock()

	return ret
}
