package ugo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/jflyup/ugo/ugo/protocol"

	"github.com/jflyup/ugo/ugo/utils"
)

var (
	errTimeout = errors.New("operation timeout")
)

// Connection is an implementation of the Conn interface for TCP network
// connections.
type Connection struct {
	conn         net.PacketConn
	connectionID protocol.ConnectionID

	addr      net.Addr
	localAddr net.Addr

	packetSender   *packetSender
	packetReceiver *packetReceiver
	segmentSender  *segmentSender

	receivedPackets  chan []byte
	sendingScheduled chan struct{}

	eof       bool
	closed    bool
	closeChan chan struct{}

	ackNoDelay    bool
	originAckTime time.Time
	linger        int

	lastRcvdPacketNumber uint32

	lastNetworkActivityTime time.Time

	timer           *time.Timer
	currentDeadline time.Time
	timerRead       bool
	fec             *FEC
	crypt           streamCrypto
	err             error
	mutex           sync.Mutex
	segmentQueue    *segmentSorter
	chRead          chan struct{}
	chWrite         chan struct{}

	dataForWriting []byte
	allSent        *sync.Cond

	readTimeout  time.Time // read deadline
	writeTimeout time.Time // write deadline

	readPosInFrame int
	writeOffset    uint64
	readOffset     uint64
	closeCallback  func()

	lastPacketNumber uint64
}

func newConnection(pc net.PacketConn, addr net.Addr, connectionID protocol.ConnectionID, crypt streamCrypto, fec *FEC, close func()) *Connection {
	c := &Connection{
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
		linger:           -1, // Graceful shutdown is default behavior
		ackNoDelay:       false,

		closeChan: make(chan struct{}, 1), // use Close(closeChan) to broadcast
		timer:     time.NewTimer(0),
		lastNetworkActivityTime: time.Now(),
		crypt: crypt,
		fec:   fec,
	}

	c.segmentSender = newSegmentSender(c) // used for outcomming segments
	return c
}

func (c *Connection) run() {
	defer c.closeCallback()

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
		case <-c.closeChan:
			return
		case <-c.timer.C:
			c.timerRead = true
		case <-c.sendingScheduled:
		case p := <-c.receivedPackets:
			err = c.handlePacket(p)
		}

		if err != nil {
			// abortive close for now
			if err.Error() == "fin" || err.Error() == "rst" {
				c.mutex.Lock()
				c.eof = true
				c.closed = true
				close(c.closeChan)
				c.mutex.Unlock()
				return
			}

			log.Println("handle error", err)
			c.closeImpl(err, false)
			return
		}
		// sendPacket may take a long time if continuous Write()
		if err := c.sendPacket(); err != nil {
			log.Println("send error", err)
			c.closeImpl(err, false)
			return
		}

		if time.Now().Sub(c.lastNetworkActivityTime) >= InitialIdleConnectionStateLifetime {
			c.closeImpl(errors.New("No recent network activity."), false)
		}
	}
}

// implementation of the net.Conn interface.
func (c *Connection) Read(p []byte) (int, error) {
	c.mutex.Lock()
	if c.eof {
		c.mutex.Unlock()
		return 0, io.EOF
	}
	c.mutex.Unlock()

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
				if frame.offset+frame.DataLen() <= c.readOffset {
					c.segmentQueue.Pop()
					frame = c.segmentQueue.Head()

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
				frame = c.segmentQueue.Head()
			case <-timeout:
				return bytesRead, errTimeout
			case <-c.closeChan:
				return bytesRead, io.ErrClosedPipe
			}
		}
		c.mutex.Unlock()

		if frame == nil {
			return bytesRead, io.EOF
		}

		m := utils.Min(len(p)-bytesRead, int(frame.DataLen())-c.readPosInFrame)
		copy(p[bytesRead:], frame.data[c.readPosInFrame:])

		c.readPosInFrame += m
		bytesRead += int(m)
		c.readOffset += uint64(m)

		//		s.flowControlManager.AddBytesRead(s.streamID, uint32(m))
		//		s.onData() // so that a possible WINDOW_UPDATE is sent
		if c.readPosInFrame >= int(frame.DataLen()) {
			c.mutex.Lock()
			c.segmentQueue.Pop()
			c.mutex.Unlock()
		}
	}

	return bytesRead, nil
}

func (c *Connection) Write(p []byte) (int, error) {
	c.mutex.Lock()
	if c.closed {
		c.mutex.Unlock()
		return 0, io.ErrClosedPipe
	}

	c.dataForWriting = make([]byte, len(p))
	copy(c.dataForWriting, p)
	c.mutex.Unlock()
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

// Close closes the connection, may block depending on LINGER option
func (c *Connection) Close() error {
	return c.closeImpl(nil, false)
}

// LocalAddr returns the local network address
func (c *Connection) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr returns the remote network address
func (c *Connection) RemoteAddr() net.Addr {
	return c.addr
}

// SetDeadline implements the Conn SetDeadline method
func (c *Connection) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}

// SetReadDeadline sets the deadline for future Read calls.
// A zero value for t means Read will not time out.
func (c *Connection) SetReadDeadline(t time.Time) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.readTimeout = t
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method
func (c *Connection) SetWriteDeadline(t time.Time) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.writeTimeout = t
	return nil
}

// SetACKNoDelay controls whether ack for packets should delay
func (c *Connection) SetACKNoDelay(nodelay bool) {
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
func (c *Connection) SetLinger(sec int) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.linger = sec
	return nil
}

// TODO timer queue
func (c *Connection) resetTimer() {
	nextDeadline := c.lastNetworkActivityTime.Add(InitialIdleConnectionStateLifetime)

	if !c.originAckTime.IsZero() {
		nextDeadline = utils.MinTime(nextDeadline, c.originAckTime.Add(protocol.AckSendDelay))
	}
	if rtoTime := c.packetSender.TimeOfFirstRTO(); !rtoTime.IsZero() {
		if rtoTime.After(time.Now()) {
			nextDeadline = utils.MinTime(nextDeadline, rtoTime)
		}
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

func (c *Connection) handlePacket(data []byte) error {
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

	if err := p.decode(); err != nil {
		log.Printf("err: %v, recv invalid data: %v from %s", err, p.rawData, c.addr.String())
		return err
	}

	if p.packetNumber != 0 {
		// need ack
		if c.originAckTime.IsZero() {
			c.originAckTime = time.Now()
		}
	}

	log.Printf("%s recv packet %d from %s, length %d", c.localAddr.String(), p.packetNumber, c.RemoteAddr().String(), p.Length)

	if p.flags == finFlag {
		log.Println("recv fin")
		// consider fin as error for quick close
		return errors.New("fin")
	}

	// no use for now
	if p.flags == (ackFlag | finFlag) {
		log.Println("recv ack fin")
		// exit the loop
		close(c.closeChan)
		return nil
	}

	if p.flags == rstFlag {
		return errors.New("rst")
	}

	if p.packetNumber != 0 {
		if err := c.packetReceiver.ReceivedPacket(p.packetNumber); err != nil {
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

func (c *Connection) handleSegment(s *segment) error {
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

func (c *Connection) handleSack(ack *sack, packetNum uint64) error {
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

func (c *Connection) closeImpl(e error, remoteClose bool) error {
	c.mutex.Lock()

	if c.closed {
		return errors.New("close closed connection")
	}

	if e != nil {
		// reset connection immediately
		c.sendRst()
		c.eof = true
		c.closed = true
		close(c.closeChan)
		return nil
	}

	if remoteClose {
		// exit immediately for now. TODO
		c.eof = true
		c.closed = true
		close(c.closeChan)
		return nil
	}
	c.mutex.Unlock()

	if e == nil && c.linger != 0 {
		// wait until all queued messages for the connection have been successfully sent(sent and acked) or
		// the timeout has been reached. kind of SO_LINGER option in TCP.
		c.allSent.L.Lock()
		for len(c.packetSender.packetHistory) != 0 {
			log.Println("closing")
			c.allSent.Wait()
		}
		c.allSent.L.Unlock()
	}
	if !remoteClose {
		c.sendFin()
	} else {
		//c.sendAckFin()
	}

	return nil
}

func (c *Connection) sendPacket() error {
	// Repeatedly try sending until no more data remained,
	// or run out of the congestion window

	// TODO send/handle packets in each goroutine?
	for {
		err := c.packetSender.CheckForError()
		if err != nil {
			return err
		}
		// do this before congestion check
		c.packetSender.checkRTO()

		if !c.packetSender.CongestionAllowsSending() {
			log.Printf("%s with %s congestion not allow, cwnd size: %d, bytes outstanding: %d",
				c.localAddr.String(), c.RemoteAddr().String(), c.packetSender.congestion.GetCongestionWindow(), c.packetSender.BytesInFlight())
			return nil
		}

		retransmitPacket := c.packetSender.DequeuePacketForRetransmission()

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
		segments := c.segmentSender.PopSegments(protocol.MaxPacketSize - 40) // TODO

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

		log.Printf("%s sending packet %d to %s\n, data length: %d", c.localAddr.String(), pkt.packetNumber, c.addr, len(pkt.rawData))
		if pkt.packetNumber != 0 {
			err = c.packetSender.SentPacket(pkt)
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

func (c *Connection) sendFin() {
	pkt := &ugoPacket{
		flags:        finFlag,
		packetNumber: 0,
	}

	pkt.encode()
	log.Printf("%s send fin to %s", c.localAddr.String(), c.RemoteAddr().String())
	c.crypt.Encrypt(pkt.rawData, pkt.rawData)
	c.conn.WriteTo(pkt.rawData, c.addr)
}

func (c *Connection) sendRst() {
	pkt := &ugoPacket{
		flags:        rstFlag,
		packetNumber: 0,
	}

	pkt.encode()
	log.Printf("%s send rst to %s", c.localAddr.String(), c.RemoteAddr().String())
	c.crypt.Encrypt(pkt.rawData, pkt.rawData)
	c.conn.WriteTo(pkt.rawData, c.addr)
}

func (c *Connection) sendAckFin() {
	pkt := &ugoPacket{
		flags:        finFlag | ackFlag,
		packetNumber: 0,
	}

	pkt.encode()
	log.Printf("%s send ack fin to %s", c.localAddr.String(), c.RemoteAddr().String())
	c.crypt.Encrypt(pkt.rawData, pkt.rawData)
	c.conn.WriteTo(pkt.rawData, c.addr)
}

// scheduleSending signals that we have data for sending
func (c *Connection) scheduleSending() {
	select {
	case c.sendingScheduled <- struct{}{}:
	default:
	}
}

func (c *Connection) lenOfDataForWriting() uint32 {
	c.mutex.Lock()
	l := uint32(len(c.dataForWriting))
	c.mutex.Unlock()
	return l
}

func (c *Connection) getDataForWriting(maxBytes uint64) []byte {
	c.mutex.Lock()
	if c.dataForWriting == nil {
		c.mutex.Unlock()
		return nil
	}
	var ret []byte
	if uint64(len(c.dataForWriting)) > maxBytes {
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
	c.writeOffset += uint64(len(ret))
	c.mutex.Unlock()

	return ret
}
