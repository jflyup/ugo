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
	// io.Copy() does not treat an EOF from Read as an error to be reported.
	if _, err := io.Copy(in, out); err != nil {
		log.Println("copy error:", err)
	}
	in.Close() // will trigger an error in the other relay which would call out.Close()
}

func handleClient(c net.Conn) {
	var addrLen uint16
	if err := binary.Read(c, binary.BigEndian, &addrLen); err != nil {
		log.Println("read length of host error:", err)
		c.Close()
		return
	}

	buf := make([]byte, addrLen)
	n, err := c.Read(buf)
	if n != int(addrLen) || err != nil {
		log.Printf("can't get original request host, err: %v", err)
		c.Close()
		return
	}
	log.Println("original request host: ", string(buf))
	proxyConn, err := net.Dial("tcp", string(buf))
	if err != nil {
		log.Println("error on Dial", err)
		c.Close()
		//proxyConn.Close() // proxyConn may be nil
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

	// Ctrl+c print call stack to debug
	// go func() {
	// 	sigs := make(chan os.Signal, 1)
	// 	signal.Notify(sigs, syscall.SIGINT)
	// 	buf := make([]byte, 1<<20)
	// 	for {
	// 		<-sigs
	// 		stacklen := runtime.Stack(buf, true)
	// 		log.Printf("=== received SIGINT ===\n*** goroutine dump...\n%s\n*** end\n", buf[:stacklen])
	// 		return
	// 	}
	// }()

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
