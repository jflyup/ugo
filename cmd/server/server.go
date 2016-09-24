package main

import (
	"io"
	//	"io/ioutil"
	"encoding/binary"
	"log"
	"net"
	"os"

	"github.com/jflyup/ugo/ugo"
	//"time"
)

func relay(in, out net.Conn) {
	if _, err := io.Copy(in, out); err != nil {
		log.Println("copy error", err)
	}
	in.Close() // will trigger an error in the other relay which would call out.Close()
}

func handleClient(c net.Conn) {
	var addrLen uint16
	if err := binary.Read(c, binary.BigEndian, &addrLen); err != nil {
		log.Println("read length of addr error:", err)
		c.Close()
		return
	}
	log.Println("length of addr :", addrLen)

	buf := make([]byte, addrLen)
	n, err := c.Read(buf)
	if n != int(addrLen) || err != nil {
		log.Printf("can't get raw addr, err: %v", err)
		c.Close()
		return
	}
	log.Println("raw addr: ", string(buf))
	proxyConn, err := net.Dial("tcp", string(buf))
	if err != nil {
		log.Println("error on Dial", err)
		c.Close()
		proxyConn.Close()
		return
	}
	go relay(c, proxyConn)
	go relay(proxyConn, c)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	f, err := os.OpenFile("proxy.log", os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Printf("error opening file: %v", err)
	}
	defer f.Close()
	log.SetOutput(f)

	//log.SetOutput(ioutil.Discard)
	if l, err := ugo.Listen("udp", os.Args[1]+":9000"); err == nil {
		for {
			if c, err := l.Accept(); err == nil {
				go handleClient(c)
			} else {
				log.Println("accept error")
			}
		}
	} else {
		log.Println("listen error", err)
		return
	}
}
