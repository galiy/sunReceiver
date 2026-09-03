package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/galiy/sunReceiver/solarman"
)

func main() {
	if len(os.Args) < 6 {
		fmt.Println("usage: probe <ip> <port> <sn hex32> <start hex16> <count hex16>")
		os.Exit(2)
	}
	ip := os.Args[1]
	port := os.Args[2]
	sn, _ := strconv.ParseUint(os.Args[3], 16, 32)
	start, _ := strconv.ParseUint(os.Args[4], 16, 16)
	count, _ := strconv.ParseUint(os.Args[5], 16, 16)

	req := solarman.BuildReadFrame(uint32(sn), uint16(start), uint16(count))
	fmt.Printf("REQ (%d bytes): %s\n", len(req), hex.EncodeToString(req))

	conn, err := net.DialTimeout("tcp", ip+":"+port, 5*time.Second)
	if err != nil {
		fmt.Println("dial error:", err)
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(req); err != nil {
		fmt.Println("write error:", err)
		os.Exit(1)
	}

	var resp []byte
	for {
		buf := make([]byte, 512)
		n, err := conn.Read(buf)
		if n > 0 {
			resp = append(resp, buf[:n]...)
		}
		if err != nil || len(resp) > 600 {
			break
		}
	}
	fmt.Printf("RESP (%d bytes): %s\n", len(resp), hex.EncodeToString(resp))
	fmt.Println("--- decoded frames ---")
	for i, f := range solarman.SplitFrames(resp) {
		fmt.Printf("frame[%d]: len=%d control=0x%04X serial=0x%04X sn=%s checksum_ok=%v payload=%s\n",
			i, len(f.Payload), f.ControlCode, f.Serial, hex.EncodeToString(
				func() []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, f.DeviceSN); return b }()),
			f.ChecksumOK, hex.EncodeToString(f.Payload))

		// try to parse as register read response
		if f.Valid && len(f.Payload) >= 16 {
			for _, p := range solarman.ParseModbusPDU(f.Payload) {
				fmt.Printf("  MODBUS resp @payload+%d: func=0x%02X bytecount=%d regs=%d\n",
					p.Offset, p.Function, p.ByteCount, len(p.Values))
				for k, v := range p.Values {
					fmt.Printf("    reg[%04X] = 0x%04X (%d)\n", uint16(start)+uint16(k), v, v)
				}
			}
		}
	}
}