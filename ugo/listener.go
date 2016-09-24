package ugo

import (
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"sync"

	"github.com/jflyup/ugo/ugo/protocol"
)

var errListenerClosed = errors.New("Listener has been closed")

type listener struct {
	sync.Cond
	mu            sync.Mutex
	err           error
	addr          net.Addr
	pending       []*Connection
	connections   map[string]*Connection
	stopListening func()
}

// Listen announces on the local network address laddr.
// The network net must be a packet-oriented netowrk:
// "udp", "udp4", "udp6", "unixgram".
func Listen(network, addr string) (net.Listener, error) {
	conn, err := net.ListenPacket(network, addr)
	if err != nil {
		return nil, err
	}
	l := &listener{
		Cond: sync.Cond{
			L: new(sync.Mutex),
		},
		addr:        conn.LocalAddr(),
		connections: make(map[string]*Connection),
	}

	// TODO implement stopListening
	go l.serve(conn)
	return l, nil
}

func (l *listener) serve(conn net.PacketConn) {
	for {
		data := make([]byte, protocol.MaxPacketSize)
		n, remoteAddr, err := conn.ReadFrom(data)
		if err != nil {
			//if !strings.HasSuffix(err.Error(), "use of closed network connection") {
			//	l.err = err
			//	log.Println("ReadFrom error", err) // TODO
			//	continue
			//}
			continue
		}

		if err := l.handlePacket(conn, remoteAddr, data[:n]); err != nil {
			log.Printf("error handling packet: %s", err.Error())
		}
	}
}

func (l *listener) handlePacket(c net.PacketConn, remoteAddr net.Addr, buffer []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if conn, ok := l.connections[remoteAddr.String()]; ok {
		// TODO check data integrity

		if len(buffer) == 22 && buffer[0] == packetInit && buffer[1] == aesEncrypt {
			log.Println("already connected, discard:", buffer)
		} else {
			// feed data to connection
			select {
			case conn.receivedPackets <- buffer:
			default:
				log.Println("queue is full, discard packet")
			}
		}
	} else {
		if len(buffer) == 22 {
			// TODO Connection migration
			if buffer[0] == packetInit {
				//iv := buffer[2:18]
				clientConnectionID := protocol.ConnectionID(binary.BigEndian.Uint32(buffer[18:22]))

				var connectionID [4]byte
				crand.Read(connectionID[:])
				c.WriteTo(connectionID[:], remoteAddr)

				// AES CBC/CFB cipher depends on the previous block of ciphertext/plaintext when encrypting/decrypting
				// so once a packet is lost, the receiver can't decrypt the following packets.
				// we should do decryption after reordering packets. Use RC4 instead for now
				key := "1234567890123456" // TODO
				RC4Crypto := newRC4Crypto([]byte(key))

				//fec = NewFEC(128, 10, 3)

				// once crypto method is settled, talk in secret
				conn := newConnection(c, remoteAddr, clientConnectionID, RC4Crypto, nil, func() {
					// TODO
					l.mu.Lock()
					delete(l.connections, remoteAddr.String())
					l.mu.Unlock()
					log.Println("close connection with ", remoteAddr.String())
				})
				l.connections[remoteAddr.String()] = conn
				log.Printf("establish connection with %s\n", remoteAddr.String())

				go conn.run()

				l.L.Lock()
				l.pending = append(l.pending, conn)
				l.L.Unlock()
				l.Signal()
			}
		}

	}
	return nil
}

// Close the listener. Stops accepting new
// connections, but will continue to process
// established connections.
func (l *listener) Close() error {
	l.L.Lock()
	err := l.err
	if err == nil {
		l.err = errListenerClosed
	}
	l.L.Unlock()
	l.Broadcast()

	if err == nil {
		l.stopListening()
	}

	return errors.New("Not implemented")
}

// Accept blocks until a new connection is ready,
// then returns that connection to the caller.
func (l *listener) Accept() (net.Conn, error) {
	l.L.Lock()
	defer l.L.Unlock()

	for l.err == nil && len(l.pending) == 0 {
		l.Wait()
	}

	if l.err != nil {
		return nil, l.err
	}

	c := l.pending[0]
	n := copy(l.pending, l.pending[1:])
	l.pending = l.pending[:n]

	return c, nil
}

// Addr returns the local address for the listener.
func (l *listener) Addr() net.Addr {
	return l.addr
}
