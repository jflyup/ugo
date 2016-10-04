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
)

var (
	errBadAddress    = errors.New("Bad peer address")
	errNotPacketConn = errors.New("Not a packet connection network")
	errConnectFailed = errors.New("Connection to remote peer failed")
)

// TODO
// DialTimeout

// DialUgo connects to the remote address raddr on the network net,
// which must be "udp", "udp4", "udp6", "unixgram".
func DialUgo(network string, laddr, raddr string) (*Conn, error) {
	pc, err := net.ListenPacket(network, laddr)
	if err != nil {
		return nil, err
	}

	var addr net.Addr
	switch network {
	case "udp", "udp4", "udp6":
		addr, err = net.ResolveUDPAddr(network, raddr)
	case "unix", "unixgram", "unixpacket":
		addr, err = net.ResolveUnixAddr(network, raddr)
	default:
		return nil, errBadAddress
	}
	if err != nil {
		return nil, err
	}

	return handshake(pc, addr)
}

// Dial connects to a remote host
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
		return nil, errBadAddress
	}
	if err != nil {
		return nil, err
	}

	return handshake(pc, addr)
}

func handshake(pc net.PacketConn, addr net.Addr) (*Conn, error) {
	var iv [16]byte
	if _, err := crand.Read(iv[:]); err != nil {
		pc.Close()
		return nil, fmt.Errorf("Failed to generate iv: %v", err)
	}
	log.Printf("addr: %s, iv: %v", pc.LocalAddr().String(), iv)
	var connectionID [4]byte
	crand.Read(connectionID[:])

	// generate INIT packet
	initPacket := &ugoPacket{
		rawData: make([]byte, 22),
	}
	initPacket.rawData[0] = byte(packetInit)
	initPacket.rawData[1] = byte(aesEncrypt)

	copy(initPacket.rawData[2:18], iv[:])
	copy(initPacket.rawData[18:22], connectionID[:])

	// negotiation
	retries := uint32(0)
	timeout := defaultRetransmissionTime
	buffer := make([]byte, 65536)
	var peerConnID uint32
	for {
		_, err := pc.WriteTo(initPacket.rawData, addr)
		if err != nil {
			pc.Close()
			return nil, err
		}

		// Exponential backoff
		timeout *= 1 << retries

		// get connection ID
		pc.SetReadDeadline(time.Now().Add(timeout))
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
			peerConnID = binary.BigEndian.Uint32(buffer[:n])
			log.Printf("connected with server")
			break
		}

		retries++
		if retries >= maxInitRetriesAttempted {
			pc.Close()
			return nil, errConnectFailed
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
func recvData(c net.PacketConn, conn *Conn) {
	for {
		buf := make([]byte, maxPacketSize)
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			conn.Close()
			return
		}

		// TODO check packet validity
		select {
		case conn.receivedPackets <- buf[:n]:
		default:
			log.Printf("discard")
		}

	}
}
