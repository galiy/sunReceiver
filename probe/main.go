package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/galiy/sunReceiver/solarman"
)

func main() {
	if len(os.Args) < 6 {
		fmt.Println("usage: probe <ip> <port> <sn hex32> [sn2 hex32...] <start hex16> <count hex16>")
		os.Exit(2)
	}
	ip := os.Args[1]
	port := os.Args[2]
	var sns []uint32
	i := 3
	for i < len(os.Args)-2 {
		v, _ := strconv.ParseUint(os.Args[i], 16, 32)
		sns = append(sns, uint32(v))
		i++
	}
	start, _ := strconv.ParseUint(os.Args[i], 16, 16)
	count, _ := strconv.ParseUint(os.Args[i+1], 16, 16)

	fmt.Printf("IP=%s port=%s start=0x%04X count=%d sns=%v\n", ip, port, start, count, sns)

	units := []uint32{1}
	if extra := os.Getenv("PROBE_UNITS"); extra != "" {
		units = units[:0]
		for _, s := range strings.Split(extra, ",") {
			v, _ := strconv.ParseUint(strings.TrimSpace(s), 16, 8)
			units = append(units, uint32(v))
		}
	}

	for _, sn := range sns {
		for _, unit := range units {
		req := solarman.BuildDeyeReadFrame(sn, unit, uint16(start), uint16(count))
		fmt.Printf("\n=== SN=0x%08X (%d) unit=0x%02X REQ (%d bytes): %s\n", sn, sn, unit, len(req), hex.EncodeToString(req))

		conn, err := net.DialTimeout("tcp", ip+":"+port, 5*time.Second)
		if err != nil {
			fmt.Println("dial error:", err)
			continue
		}
		conn.SetDeadline(time.Now().Add(12 * time.Second))
		if _, err := conn.Write(req); err != nil {
			fmt.Println("write error:", err)
			conn.Close()
			continue
		}

		var resp []byte
		for {
			buf := make([]byte, 512)
			n, err := conn.Read(buf)
			if n > 0 {
				resp = append(resp, buf[:n]...)
			}
			if err != nil || len(resp) > 1200 {
				break
			}
		}
		conn.Close()
		fmt.Printf("RESP (%d bytes): %s\n", len(resp), hex.EncodeToString(resp))

		frames := solarman.SplitFrames(resp)
		for j, f := range frames {
			fmt.Printf("  frame[%d]: len=%d control=0x%04X sn=%s cksum_ok=%v\n",
				j, len(f.Payload), f.ControlCode, hex.EncodeToString(
					func() []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, f.DeviceSN); return b }()),
				f.ChecksumOK)
			if code, ok := solarman.DeyeErrorCode(f); ok {
				switch code {
				case 0x06:
					fmt.Println("    Deye error: 0x06 Logger Serial Number does not match")
				case 0x05:
					fmt.Println("    Deye error: 0x05 Modbus device address does not match")
				default:
					fmt.Printf("    Deye error: 0x%02X\n", code)
				}
			}
			for _, p := range solarman.ParseModbusPDU(f.Payload) {
				fmt.Printf("    MODBUS func=0x%02X bytecount=%d regs=%d crc_ok=%v\n",
					p.Function, p.ByteCount, len(p.Values), p.CRC == p.CRCCalc)
				for k, v := range p.Values {
					fmt.Printf("      reg[0x%03X] = 0x%04X (%d)\n", uint16(start)+uint16(k), v, v)
				}
			}
		}
	}
	}
}