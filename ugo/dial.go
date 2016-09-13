package ugo

import (
	"encoding/binary"
	"log"
	//"bytes"
	crand "crypto/rand"
	"errors"
	"fmt"
	"net"
	"time"

	"./protocol"
)

var (
	ErrBadAddress    = errors.New("Bad peer address")
	ErrNotPacketConn = errors.New("Not a packet connection network")
	ErrConnectFailed = errors.New("Connection to remote peer failed")
)

// TODO
// DialTimeout

// Connects to a remote host
// The network net must be a packet-oriented netowrk:
// "udp", "udp4", "udp6", "unixgram".
func Dial(network, address string) (net.Conn, error) {
	pc, err := net.ListenPacket(network, "")
	if err != nil {
		return nil, err
	}

	var addr net.Addr
	switch network {
	case "udp", "udp4", "udp6":
		addr, err = net.ResolveUDPAddr(network, address)
	case "unix", "unixgram", "unixpacket":
		addr, err = net.ResolveUnixAddr(network, address)
	default:
		return nil, ErrBadAddress
	}
	if err != nil {
		return nil, err
	}

	return handshake(pc, addr)
}

func handshake(pc net.PacketConn, addr net.Addr) (net.Conn, error) {
	var iv [16]byte
	if _, err := crand.Read(iv[:]); err != nil {
		pc.Close()
		return nil, fmt.Errorf("Failed to generate iv: %v", err)
	}
	log.Printf("addr: %s, iv: %v", pc.LocalAddr().String(), iv)
	var connectionID [4]byte
	crand.Read(connectionID[:])

	// generate INIT packet
	initPacket := &Packet{
		D: make([]byte, 22),
	}
	initPacket.D[0] = byte(PacketInit)
	initPacket.D[1] = byte(AESEncrypt)

	copy(initPacket.D[2:18], iv[:])
	copy(initPacket.D[18:22], connectionID[:])

	// negotiation
	retries := 5
	buffer := make([]byte, 65536)
	var peerConnID protocol.ConnectionID
	for {
		_, err := pc.WriteTo(initPacket.D, addr)
		if err != nil {
			pc.Close()
			return nil, err
		}

		// wait for a response
		pc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _, err := pc.ReadFrom(buffer[:cap(buffer)])
		pc.SetReadDeadline(time.Time{})
		if ne, ok := err.(net.Error); ok && ne.Timeout() && ne.Temporary() {
			n = 0
		} else if err != nil {
			pc.Close()
			return nil, err
		}

		// did we get reply?
		if n == 4 {
			peerConnID = protocol.ConnectionID(binary.BigEndian.Uint32(buffer[:n]))
			log.Printf("connected with server, retry %d times", retries)
			break
		}

		retries--
		if retries == 0 {
			pc.Close()
			return nil, ErrConnectFailed
		}
	}

	const key = "1234567890123456"
	RC4Crypto := newRC4Crypto([]byte(key))

	//fec = NewFEC(128, 10, 3)
	// create connection
	conn := newConnection(pc, addr, peerConnID, RC4Crypto, nil, func() { pc.Close() })

	go conn.run()
	go recvData(pc, conn)
	return conn, nil
}

// receive packets and feed them to the connection
func recvData(c net.PacketConn, conn *connection) {
	for {
		buf := make([]byte, protocol.MaxPacketSize)
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			conn.Close()
			return
		}

		if n > 4 { // TODO check packet validity
			conn.receivedPackets <- buf[:n]
		} else {
			log.Printf("recv malformed data")
		}
	}
}
