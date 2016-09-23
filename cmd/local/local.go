package main

import (
	"encoding/binary"
	"errors"
	"io"
	//	"io/ioutil"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"

	"github.com/jflyup/ugo/ugo"
	//"time"
)

const (
	IPv4       = 1
	DomainName = 3
	IPv6       = 4

	Connect   = 1
	Bind      = 2
	Associate = 3

	// The maximum packet size of any udp Associate packet, based on ethernet's max size,
	// minus the IP and UDP headers. IPv4 has a 20 byte header, UDP adds an
	// additional 4 bytes.  This is a total overhead of 24 bytes.  Ethernet's
	// max packet size is 1500 bytes,  1500 - 24 = 1476.
	maxUDPPacketSize = 1476
)

const (
	succeeded uint8 = iota
	serverFailure
	notAllowed
	networkUnreachable
	hostUnreachable
	connectionRefused
	TTLExpired
	commandNotSupported
	addrTypeNotSupported
)

func handleRequest(c net.Conn) {
	/*
		The SOCKS request is formed as follows:
		+----+-----+-------+------+----------+----------+
		|VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
		+----+-----+-------+------+----------+----------+
		| 1  |  1  | X'00' |  1   | Variable |    2     |
		+----+-----+-------+------+----------+----------+
	*/
	header := make([]byte, 3)

	_, err := io.ReadFull(c, header)

	if err != nil {
		log.Println("illegal request", err)
		c.Close()
		return
	}

	switch header[1] {
	case Connect:
		handleConnect(c)
	case Bind:
		handleBind(c)
	case Associate:
		handleUDP(c)
	default:
		sendReply(c, commandNotSupported)
		c.Close()
	}
}

func sendReply(c net.Conn, rep uint8) {
	reply := []byte{
		5,
		rep,
		0,
		1,
	}

	localAddr := c.LocalAddr().String()
	localHost, localPort, _ := net.SplitHostPort(localAddr)
	ipBytes := net.ParseIP(localHost).To4()
	nPort, _ := strconv.Atoi(localPort)
	reply = append(reply, ipBytes...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(nPort))
	reply = append(reply, portBytes...)

	c.Write(reply)
}

func doConnect(c net.Conn, command uint8) (proxyConn net.Conn, err error) {
	addrType := make([]byte, 1)
	c.Read(addrType)
	var host string
	switch addrType[0] {
	case IPv4:
		ipv4 := make(net.IP, net.IPv4len)
		c.Read(ipv4)
		host = ipv4.String()
	case IPv6:
		ipv6 := make(net.IP, net.IPv6len)
		c.Read(ipv6)
		host = ipv6.String()
	case DomainName:
		var domainLen uint8
		binary.Read(c, binary.BigEndian, &domainLen)
		domain := make([]byte, domainLen)
		c.Read(domain)
		host = string(domain)
	default:
		sendReply(c, addrTypeNotSupported)
		err = errors.New("Address type not supported")
		return nil, err
	}

	var port uint16
	binary.Read(c, binary.BigEndian, &port)

	// connect to host
	addr := net.JoinHostPort(host, strconv.Itoa(int(port)))

	//timeout := time.Duration(10 * time.Second)
	client, err := ugo.Dial("udp", os.Args[1]+":9000")

	if err != nil {
		log.Println("fail to connect to ugo server", err)
		sendReply(c, serverFailure)
		return nil, err
	}

	sendReply(c, succeeded)
	binary.Write(client, binary.BigEndian, uint16(len(addr)))
	client.Write([]byte(addr))
	return client, nil
}

func handleConnect(c net.Conn) {
	proxyConn, err := doConnect(c, Connect)
	if err != nil {
		c.Close()
	} else {
		go relay(c, proxyConn)
		go relay(proxyConn, c)
	}

}

func relay(in, out net.Conn) {
	if _, err := io.Copy(in, out); err != nil {
		log.Println("copy error", err)
	}
	in.Close() // will trigger an error in the other relay which will call out.Close()
}

// passive mode
func handleBind(c net.Conn) {
}

func handleUDP(c net.Conn) {
	log.Println("UDP Associate")
	/*
	   +----+------+------+----------+----------+----------+
	   |RSV | FRAG | ATYP | DST.ADDR | DST.PORT |   DATA   |
	   +----+------+------+----------+----------+----------+
	   | 2  |  1   |  1   | Variable |    2     | Variable |
	   +----+------+------+----------+----------+----------+
	*/
	buf := make([]byte, 3)
	c.Read(buf)
	// relay udp datagram silently, without any notification to the requesting client
	if buf[2] != 0 {
		// does not support fragmentation, drop it
		log.Println("does not support fragmentation, drop")
		dummy := make([]byte, maxUDPPacketSize)
		c.Read(dummy)
	}

	proxyConn, err := doConnect(c, Associate)
	if err != nil {
		c.Close()
	} else {
		go relay(c, proxyConn)
		go relay(proxyConn, c)
	}
}

func handleNewConn(c net.Conn) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		log.Println("negotiation err", err)
		c.Close()
		return
	}

	if version := buf[0]; version != 5 {
		log.Printf("only support socks5, request from: ", c.RemoteAddr())
		c.Close()
		return
	}
	nMethods := buf[1]

	methods := make([]byte, nMethods)
	if len, err := c.Read(methods); len != int(nMethods) || err != nil {
		log.Println("wrong method")
		c.Close()
		return
	}

	// no authentication required for now
	buf[1] = 0

	// send a METHOD selection message
	c.Write(buf)

	handleRequest(c)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	f, err := os.OpenFile("proxy.log", os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		fmt.Printf("error opening file: %v", err)
	}
	defer f.Close()
	log.SetOutput(f)

	//log.SetOutput(ioutil.Discard)

	l, err := net.Listen("tcp", ":1080")
	if err != nil {
		log.Fatal("listen error", err)
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal("accept error", err)
		}

		go handleNewConn(conn)
	}
}
