package main

import (
	"fmt"
	"time"
	//	"io/ioutil"
	"log"
	"net"
	"os"

	"github.com/jflyup/ugo/ugo"
)

func handleClient(c net.Conn) {
	buf := make([]byte, 2000)

	//c.SetDeadline(time.Now().Add(10 * time.Second))
	var sum int
	for {
		if n, err := c.Read(buf); err == nil {
			sum += n
			c.Write([]byte("hello, client"))

		} else {
			fmt.Println(err)
			break
		}
	}
	c.Close()
	fmt.Printf("recv %d bytes in total\n", sum)
}

func client() {
	start := time.Now()
	client, err := ugo.Dial("udp", "127.0.0.1:9000")
	if err != nil {
		fmt.Println(err)
		return
	}
	//client.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 1000)
	for i := 0; i < len(buf); i++ {
		buf[i] = 95
	}

	var sum int
	for i := 0; i < 10000; i++ {
		if _, err := client.Write(buf); err != nil {
			fmt.Println("error writing, exiting", err)
			break
		}

		if n, err := client.Read(buf); err == nil {
			sum += n
		}
	}

	client.Close()
	end := time.Now()
	fmt.Printf("client recv response, %d bytes in %f seconds\n", sum, end.Sub(start).Seconds())
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

	if l, err := ugo.Listen("udp", "0.0.0.0:9000"); err == nil {
		go client()
		for {
			if c, err := l.Accept(); err == nil {
				go handleClient(c)
			} else {
				fmt.Println("accept error")
			}
		}
	}

}
