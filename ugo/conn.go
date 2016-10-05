package ugo

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jflyup/ugo/ugo/utils"
	"github.com/lucas-clemente/quic-go/protocol"
)

var (
	errTimeout = errors.New("operation timeout")
)

// Conn is an implementation of the Conn interface for
// reliable udp network connections. Use it like net.TcpConn
type Conn struct {
	conn         net.PacketConn
	connectionID uint32

	addr      net.Addr
	localAddr net.Addr

	packetSender   *packetSender
	packetReceiver *packetReceiver
	segmentSender  *segmentSender

	receivedPackets chan []byte
	chWriteEvent    chan struct{}

	eof       int32
	closed    int32
	finSent   bool
	finRcvd   bool
	closeChan chan struct{}

	ackNoDelay    bool
	originAckTime time.Time
	linger        int
	lingerTimer   *time.Timer

	lastRcvdPacketNumber uint32

	lastNetworkActivityTime time.Time

	timer           *time.Timer
	currentDeadline time.Time
	timerRead       bool
	fec             *FEC // forward error correction, good for lossy links but not for congested ones
	crypt           streamCrypto
	err             error
	mutex           sync.Mutex
	segmentQueue    *segmentSorter
	chRead          chan struct{}
	chWriteDone     chan struct{}

	dataForWriting []byte

	readTimeout  time.Time // read deadline
	writeTimeout time.Time // write deadline

	readPosInFrame int
	writeOffset    uint64
	readOffset     uint64
	closeCallback  func()

	lastPacketNumber uint64
}

func newConnection(pc net.PacketConn, addr net.Addr, connectionID uint32, crypt streamCrypto, fec *FEC, close func()) *Conn {
	c := &Conn{
		connectionID:  connectionID,
		conn:          pc,
		addr:          addr,
		localAddr:     pc.LocalAddr(),
		closeCallback: close,

		packetSender:   newPacketSender(),
		packetReceiver: newPacketReceiver(),

		segmentQueue: newSegmentSorter(), // used for incoming segments reordering

		receivedPackets: make(chan []byte, 1024),
		chWriteEvent:    make(chan struct{}, 1),
		chRead:          make(chan struct{}, 1),
		chWriteDone:     make(chan struct{}, 1),
		linger:          -1, // Graceful shutdown is default behavior
		ackNoDelay:      false,

		closeChan: make(chan struct{}, 1), // use Close(closeChan) to broadcast
		timer:     time.NewTimer(0),
		lastNetworkActivityTime: time.Now(),
		crypt: crypt,
		fec:   fec,
	}

	c.segmentSender = newSegmentSender(c) // used for outcomming segments
	return c
}

func (c *Conn) run() {
	defer c.closeCallback()

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
		case <-c.closeChan:
			return
		case <-c.timer.C:
			c.timerRead = true
		case <-c.chWriteEvent:
		case p := <-c.receivedPackets:
			err = c.handlePacket(p)
		}

		if err != nil {
			log.Println("handle error:", err)
			c.resetConn(err, false)
			return
		}
		// sendPacket may take a long time if continuous Write()
		if err = c.sendPacket(); err != nil {
			log.Println("send error:", err)
			c.resetConn(err, false)
			return
		}

		if time.Now().Sub(c.lastNetworkActivityTime) >= initialIdleConnectionStateLifetime {
			c.resetConn(errors.New("No recent network activity."), false)
		}
	}
}

// implementation of the net.Conn interface.
func (c *Conn) Read(p []byte) (int, error) {
	if atomic.LoadInt32(&c.eof) == 1 {
		return 0, io.EOF
	}

	bytesRead := 0
	for bytesRead < len(p) {
		c.mutex.Lock()
		frame := c.segmentQueue.head()

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
				if frame.dataLen() == 0 {
					log.Printf("EOF:%s<--%s", c.localAddr.String(), c.RemoteAddr().String())
					c.mutex.Unlock()
					atomic.StoreInt32(&c.eof, 1)
					return bytesRead, io.EOF
				}
				// Pop and continue if the frame doesn't have any new data
				if frame.offset+frame.dataLen() <= c.readOffset {
					c.segmentQueue.pop()
					frame = c.segmentQueue.head()
					continue
				}
				// If the frame's offset is <= our current read pos, and we didn't
				// go into the previous if, we can read data from the frame.
				if frame.offset <= c.readOffset {
					// Set our read position in the frame properly
					c.readPosInFrame = int(c.readOffset - frame.offset)
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
				frame = c.segmentQueue.head()
			case <-timeout:
				return bytesRead, errTimeout
			case <-c.closeChan:
				return bytesRead, io.ErrClosedPipe
			}
		}
		c.mutex.Unlock()

		// nearly impossible, check it anyway
		if frame == nil {
			atomic.StoreInt32(&c.eof, 1)
			return bytesRead, io.EOF
		}

		m := utils.Min(len(p)-bytesRead, int(frame.dataLen())-c.readPosInFrame)
		copy(p[bytesRead:], frame.data[c.readPosInFrame:])

		c.readPosInFrame += m
		bytesRead += int(m)
		c.readOffset += uint64(m)

		//		s.flowControlManager.AddBytesRead(s.streamID, uint32(m))
		//		s.onData() // so that a possible WINDOW_UPDATE is sent
		if c.readPosInFrame >= int(frame.dataLen()) {
			c.mutex.Lock()
			c.segmentQueue.pop()
			c.mutex.Unlock()
			if frame.dataLen() == 0 {
				atomic.StoreInt32(&c.eof, 1)
				return bytesRead, io.EOF
			}
		}
	}

	return bytesRead, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		return 0, io.ErrClosedPipe
	}

	c.mutex.Lock()
	// TODO don't make every time
	c.dataForWriting = make([]byte, len(p))
	copy(c.dataForWriting, p)
	c.mutex.Unlock()
	c.notifyWriteEvent()

	var timeout <-chan time.Time
	if !c.writeTimeout.IsZero() {
		delay := c.writeTimeout.Sub(time.Now())
		timeout = time.After(delay)
	}

	for {
		select {
		case <-c.chWriteDone:
			return len(p), nil
		case <-timeout:
			return 0, errTimeout
		case <-c.closeChan:
			return 0, io.ErrClosedPipe
		}
	}
}

// Close closes the connection
func (c *Conn) Close() error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return errors.New("close closed connection")
	}
	if c.linger == 0 {
		c.resetConn(nil, false)
		return nil
	}
	// no more Write()
	atomic.StoreInt32(&c.closed, 1)

	// Close can be also treat as a write event
	c.notifyWriteEvent()
	if c.linger > 0 {
		c.lingerTimer = time.AfterFunc(time.Duration(c.linger)*time.Second,
			func() { c.resetConn(errors.New("linger timeout"), false) })
	}
	return nil
}

// LocalAddr returns the local network address
func (c *Conn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr returns the remote network address
func (c *Conn) RemoteAddr() net.Addr {
	return c.addr
}

// SetDeadline implements the Conn SetDeadline method
func (c *Conn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

// SetReadDeadline sets the deadline for future Read calls.
// A zero value for t means Read will not time out.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.readTimeout = t
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.writeTimeout = t
	return nil
}

// SetACKNoDelay controls whether ack for packets should delay
func (c *Conn) SetACKNoDelay(nodelay bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.ackNoDelay = nodelay
}

// SetLinger sets the behavior of Close on a connection which still
// has data waiting to be sent or to be acknowledged.
//
// If sec < 0 (the default), wait for pending data to be sent before closing the connection
// If sec == 0, discard any unsent or unacknowledged data.
//
// If sec > 0, the data is sent in the background as with sec < 0.
// after sec seconds have elapsed any remaining
// unsent data will be discarded.
func (c *Conn) SetLinger(sec int) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.linger = sec
	return nil
}

// TODO SetKeepAlive

// TODO timer queue
func (c *Conn) resetTimer() {
	nextDeadline := c.lastNetworkActivityTime.Add(initialIdleConnectionStateLifetime)

	if !c.originAckTime.IsZero() {
		nextDeadline = utils.MinTime(nextDeadline, c.originAckTime.Add(protocol.AckSendDelay))
	}
	if rtoTime := c.packetSender.timeOfFirstRTO(); !rtoTime.IsZero() {
		if rtoTime.After(time.Now()) {
			nextDeadline = utils.MinTime(nextDeadline, rtoTime)
		}
	}

	if nextDeadline.Equal(c.currentDeadline) {
		// No need to reset the timer
		return
	}

	// To reuse an active timer,
	// always call its Stop method first
	// and—if it had expired—drain the value from its channel.
	if !c.timer.Stop() && !c.timerRead {
		<-c.timer.C
	}
	c.timer.Reset(nextDeadline.Sub(time.Now()))

	c.timerRead = false
	c.currentDeadline = nextDeadline
}

func (c *Conn) handlePacket(data []byte) error {
	c.lastNetworkActivityTime = time.Now()

	c.crypt.Decrypt(data, data)
	// TODO check data integrity
	if c.fec != nil {
		// TODO
		f := c.fec.decode(data)
		if f.Flag() == typeData || f.Flag() == typeFEC {
			if recovered := c.fec.input(f); recovered != nil {
				for k := range recovered {
					log.Println("recovered:", binary.LittleEndian.Uint32(recovered[k]))
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

	if err := p.decode(); err != nil {
		log.Printf("err: %v, recv invalid data: %v from %s", err, p.rawData, c.addr.String())
		return err
	}

	if p.packetNumber != 0 {
		if c.originAckTime.IsZero() {
			c.originAckTime = time.Now()
		}
	}

	//log.Printf("%s recv packet %d from %s, length %d", c.localAddr.String(), p.packetNumber, c.RemoteAddr().String(), p.Length)

	if p.flags&finFlag == finFlag {
		log.Printf("%s recv fin", c.LocalAddr().String())
		c.finRcvd = true
	}
	// no ack for fin at present
	if p.flags == (ackFlag | finFlag) {
		log.Println("recv ack fin")
		// exit the loop
		c.exitLoop()
		return nil
	}

	if p.flags == rstFlag {
		c.resetConn(errors.New("Connection reset by peer"), true)
		return nil
	}

	if p.packetNumber != 0 {
		if err := c.packetReceiver.receivedPacket(p.packetNumber); err != nil {
			return err
		}
	}

	if p.sack != nil {
		log.Printf("%s recv ack from %s: %v", c.localAddr.String(), c.RemoteAddr().String(), p.sack)
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
		log.Printf("%s recv stop waiting %d from %s", c.localAddr.String(), p.stopWaiting, c.addr.String())
		c.packetReceiver.receivedStopWaiting(p.stopWaiting)
	}

	return nil
}

func (c *Conn) handleSegment(s *segment) error {
	c.mutex.Lock()

	err := c.segmentQueue.push(s)
	if err != nil && err != errDuplicateStreamData {
		c.mutex.Unlock()
		return err
	}
	c.mutex.Unlock()

	// non-blocking
	select {
	case c.chRead <- struct{}{}:
	default:
	}

	return nil
}

func (c *Conn) handleSack(ack *sack, packetNum uint64) error {
	if err := c.packetSender.receivedAck(ack, packetNum); err != nil {
		return err
	}

	return nil
}

func (c *Conn) resetConn(e error, remoteClosed bool) error {
	log.Printf("reason of reset: %v", e)
	if !remoteClosed {
		c.sendRst()
	}

	atomic.StoreInt32(&c.eof, 1)
	atomic.StoreInt32(&c.closed, 1)
	// TODO exit loop ASAP
	c.exitLoop()

	return nil
}

func (c *Conn) sendPacket() error {
	// Repeatedly try sending until no more data remained,
	// or run out of the congestion window

	// TODO send/handle packets in each goroutine?
	// sending loop may lead to spurious RTO since ack arrives
	// but sender didn't handle in time
	for {
		// don't know if short circuit is guaranteed by the go spec
		if atomic.LoadInt32(&c.closed) == 1 {
			if c.lenOfDataForWriting() == 0 && len(c.packetSender.packetHistory) == 0 && !c.finSent {
				c.sendFin()
				// stop linger timer if any
				if c.lingerTimer != nil {
					if !c.lingerTimer.Stop() {
						<-c.lingerTimer.C
					}
				}
				// after fin was sent, no more packets except ACKs would be sent
			}
			// application may not read the EOF, so if we received fin
			// and application asks to close the connection, exit the loop
			if c.finSent && c.finRcvd {
				c.exitLoop()
				return nil
			}
		}
		err := c.packetSender.CheckForError()
		if err != nil {
			return err
		}
		// do this before congestion check since it may alter CWND
		c.packetSender.checkRTO()

		if !c.packetSender.CongestionAllowsSending() {
			log.Printf("%s with %s congestion not allow, cwnd size: %d, bytes outstanding: %d",
				c.localAddr.String(), c.RemoteAddr().String(), c.packetSender.congestion.GetCongestionWindow(), c.packetSender.BytesInFlight())
			return nil
		}

		retransmitPacket := c.packetSender.dequeuePacketForRetransmission()

		if retransmitPacket != nil {
			// if retransmitted packet contains control message
			if retransmitPacket.flags&ackFlag == ackFlag {
				c.packetReceiver.stateChanged = true
			}
			if retransmitPacket.flags&stopFlag == stopFlag {
				c.packetSender.stopWaitingManager.state = true
			}

			for _, seg := range retransmitPacket.segments {
				log.Println("retransmit segment", seg.offset)
				c.segmentSender.AddSegmentForRetransmission(seg)
			}
		}

		// TODO function pack()
		ack, err := c.packetReceiver.buildSack(false)
		if err != nil {
			return err
		}

		stopWait := c.packetSender.GetStopWaitingFrame()

		// get data
		segments := c.segmentSender.PopSegments(maxPacketSize - 40) // TODO

		if ack == nil && len(segments) == 0 && stopWait == 0 {
			return nil
		}

		// Check whether we are allowed to send a packet containing only an ACK
		onlyAck := time.Now().Sub(c.originAckTime) > protocol.AckSendDelay || c.ackNoDelay

		if len(segments) == 0 && stopWait == 0 {
			if !onlyAck {
				return nil
			}
		}

		// Pop the ACK frame now that we are sure we're gonna send it
		if ack != nil {
			_, err = c.packetReceiver.buildSack(true)
			if err != nil {
				return err
			}
		}

		if len(segments) != 0 || stopWait != 0 {
			c.lastPacketNumber++
		}

		pkt := &ugoPacket{
			packetNumber: c.lastPacketNumber,
			sack:         ack,
			segments:     segments,
			stopWaiting:  stopWait,
		}

		if err := pkt.encode(); err != nil {
			log.Println("encode error, packet:", pkt)
			return err
		}

		if pkt.flags == ackFlag {
			pkt.packetNumber = 0
		}

		//log.Printf("%s sending packet %d to %s\n, data length: %d", c.localAddr.String(), pkt.packetNumber, c.addr, len(pkt.rawData))
		if pkt.packetNumber != 0 {
			err = c.packetSender.sentPacket(pkt)
			if err != nil {
				return err
			}

		}

		c.originAckTime = time.Time{}

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

func (c *Conn) sendFin() {
	pkt := &ugoPacket{
		flags:        finFlag,
		packetNumber: 0,
	}
	seg := &segment{
		offset: c.writeOffset,
	}
	pkt.segments = append(pkt.segments, seg)

	pkt.encode()
	log.Printf("%s send fin to %s", c.localAddr.String(), c.RemoteAddr().String())
	c.crypt.Encrypt(pkt.rawData, pkt.rawData)
	c.conn.WriteTo(pkt.rawData, c.addr)
	c.finSent = true
}

func (c *Conn) sendRst() {
	pkt := &ugoPacket{
		flags:        rstFlag,
		packetNumber: 0,
	}

	pkt.encode()
	log.Printf("%s send rst to %s", c.localAddr.String(), c.RemoteAddr().String())
	c.crypt.Encrypt(pkt.rawData, pkt.rawData)
	c.conn.WriteTo(pkt.rawData, c.addr)
}

func (c *Conn) sendAckFin() {
	pkt := &ugoPacket{
		flags:        finFlag | ackFlag,
		packetNumber: 0,
	}

	pkt.encode()
	log.Printf("%s send ack fin to %s", c.localAddr.String(), c.RemoteAddr().String())
	c.crypt.Encrypt(pkt.rawData, pkt.rawData)
	c.conn.WriteTo(pkt.rawData, c.addr)
}

func (c *Conn) exitLoop() {
	select {
	case <-c.closeChan:
		// closed
	default:
		// not closed, close it
		close(c.closeChan)
	}
}

// writeEvent signals that we have data for sending
func (c *Conn) notifyWriteEvent() {
	select {
	case c.chWriteEvent <- struct{}{}:
	default:
	}
}

func (c *Conn) doneWriting() {
	select {
	case c.chWriteDone <- struct{}{}:
	default:
	}
}

func (c *Conn) lenOfDataForWriting() uint32 {
	c.mutex.Lock()
	l := uint32(len(c.dataForWriting))
	c.mutex.Unlock()
	return l
}

func (c *Conn) getDataForWriting(maxBytes uint64) []byte {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.dataForWriting == nil {
		return nil
	}
	var ret []byte
	if uint64(len(c.dataForWriting)) > maxBytes {
		ret = c.dataForWriting[:maxBytes]
		c.dataForWriting = c.dataForWriting[maxBytes:]
	} else {
		ret = c.dataForWriting
		c.dataForWriting = nil
		c.doneWriting()
	}

	c.writeOffset += uint64(len(ret))

	return ret
}
