// Command protectprobe sends one SW-P-08 protect message to a router and
// prints every reply as raw hex for a few seconds. It is a field-test tool.
//
//	protectprobe -addr 127.0.0.1:8910 -op lock -dst 25
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/sethdotfm/sode-vep/internal/swp08"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8910", "router host:port")
	op := flag.String("op", "ask", "ask (0x8A), lock (0x8C), unlock (0x8E) or dump (0x93)")
	dst := flag.Int("dst", 0, "wire destination, 0-based")
	dev := flag.Int("device", 0, "device number sent with lock and unlock")
	wait := flag.Duration("wait", 2*time.Second, "how long to print replies")
	flag.Parse()

	var m []byte
	switch *op {
	case "ask":
		m = swp08.ExtProtectInterrogate(0, 0, *dst)
	case "lock":
		m = swp08.ExtProtectConnect(0, 0, *dst, *dev)
	case "unlock":
		m = swp08.ExtProtectDisconnect(0, 0, *dst, *dev)
	case "dump":
		m = swp08.ExtProtectDumpReq(0, 0, 0)
	default:
		fmt.Fprintln(os.Stderr, "unknown -op", *op)
		os.Exit(2)
	}
	conn, err := net.Dial("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	fmt.Printf("tx  % X\n", m)
	if _, err := conn.Write(swp08.Encode(m)); err != nil {
		log.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(*wait))
	var d swp08.Decoder
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		d.Feed(buf[:n], func(f swp08.Frame) {
			at := time.Since(start).Round(time.Millisecond)
			switch f.Kind {
			case swp08.KindAck:
				fmt.Printf("%6s ACK\n", at)
			case swp08.KindNak:
				fmt.Printf("%6s NAK\n", at)
			case swp08.KindMsg:
				fmt.Printf("%6s rx  % X\n", at, f.Msg)
				if f.Msg[0] != swp08.CmdExtProtectDump {
					return
				}
				// Only protected entries, so a 72-output dump stays short.
				for i := 0; 6+i*2+1 < len(f.Msg); i++ {
					if hi := f.Msg[6+i*2]; hi&0x30 != 0 {
						first := int(f.Msg[4])<<8 | int(f.Msg[5])
						fmt.Printf("       dst %d state %d device %d\n", first+i, hi>>4&3, int(hi&0x0F)<<8|int(f.Msg[7+i*2]))
					}
				}
			}
		})
	}
}
