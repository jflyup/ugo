package main

import (
	"io"
	//	"io/ioutil"
	"log"
	"net"
	"os"

	"../../ugo"
	//"time"
)

func relay(in, out net.Conn) {
	if _, err := io.Copy(in, out); err != nil {
		log.Println("copy error", err)
	}
	in.Close() // will trigger an error in the other relay which will call out.Close()
}

func handleClient(c net.Conn) {
	buf := make([]byte, 512)
	n, _ := c.Read(buf)
	log.Println("raw addr: ", string(buf[:n]))
	if proxyConn, err := net.Dial("tcp", string(buf[:n])); err != nil {
		log.Println("error on Dial", err)
		c.Close()
		return
	} else {
		go relay(c, proxyConn)
		go relay(proxyConn, c)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	f, err := os.OpenFile("proxy.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Println("error opening file: %v", err)
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
