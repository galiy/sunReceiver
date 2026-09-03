package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

func crc16Modbus(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func checksum8(b []byte) byte {
	sum := 0
	for _, x := range b {
		sum += int(x)
	}
	return byte(sum & 0xff)
}

func buildReadFrame(sn uint32, start, count uint16) []byte {
	pdu := make([]byte, 6)
	pdu[0] = 0x01
	pdu[1] = 0x03
	binary.BigEndian.PutUint16(pdu[2:4], start)
	binary.BigEndian.PutUint16(pdu[4:6], count)
	crc := crc16Modbus(pdu)

	payload := []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	payload = append(payload, pdu...)
	payload = append(payload, byte(crc), byte(crc>>8))

	frame := []byte{0xA5}
	frame = append(frame, byte(len(payload)), byte(len(payload)>>8))
	frame = append(frame, 0x10, 0x45)
	frame = append(frame, 0x00, 0x00)
	frame = append(frame, byte(sn), byte(sn>>8), byte(sn>>16), byte(sn>>24))
	frame = append(frame, payload...)
	frame = append(frame, checksum8(frame[1:]))
	frame = append(frame, 0x15)
	return frame
}

type frame struct {
	payloadLen uint16
	control    uint16
	serial     uint16
	sn         uint32
	payload    []byte
	checksum   byte
	valid      bool
}

func splitFrames(raw []byte) []frame {
	var frames []frame
	i := 0
	for i < len(raw) {
		if raw[i] != 0xA5 {
			i++
			continue
		}
		// find end marker: next A5 is start of next frame, or use length
		if i+6 > len(raw) {
			break
		}
		plen := binary.LittleEndian.Uint16(raw[i+1 : i+3])
		total := 11 + int(plen) + 2
		if i+total > len(raw) {
			// fall back: find next A5
			nxt := i + 1
			for nxt < len(raw) && raw[nxt] != 0xA5 {
				nxt++
			}
			if nxt >= len(raw) {
				break
			}
			frames = append(frames, frame{})
			i = nxt
			continue
		}
		f := frame{
			payloadLen: plen,
			control:    binary.LittleEndian.Uint16(raw[i+3 : i+5]),
			serial:     binary.BigEndian.Uint16(raw[i+5 : i+7]),
			sn:         binary.LittleEndian.Uint32(raw[i+7 : i+11]),
			payload:    raw[i+11 : i+11+int(plen)],
			checksum:   raw[i+11+int(plen)],
		}
		expected := checksum8(raw[i+1 : i+11+int(plen)])
		f.valid = f.checksum == expected && raw[i+total-1] == 0x15
		frames = append(frames, f)
		i += total
	}
	return frames
}

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

	frame := buildReadFrame(uint32(sn), uint16(start), uint16(count))
	fmt.Printf("REQ (%d bytes): %s\n", len(frame), hex.EncodeToString(frame))

	conn, err := net.DialTimeout("tcp", ip+":"+port, 5*time.Second)
	if err != nil {
		fmt.Println("dial error:", err)
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(frame); err != nil {
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
	for i, f := range splitFrames(resp) {
		fmt.Printf("frame[%d]: len=%d control=0x%04X serial=0x%04X sn=%s checksum_ok=%v payload=%s\n",
			i, f.payloadLen, f.control, f.serial, hex.EncodeToString(
				func() []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, f.sn); return b }()),
			f.valid, hex.EncodeToString(f.payload))

		// try to parse as register read response
		if f.valid && len(f.payload) >= 16 {
			// frameType, status, delivery(4), poweron(4), offset(4) = 14
			bp := f.payload[14:]
			// search for modbus response: addr(01) func(03|04) bytecount data crc
			for j := 0; j+5 <= len(bp); j++ {
				if bp[j] == 0x01 && (bp[j+1] == 0x03 || bp[j+1] == 0x04) {
					bc := int(bp[j+2])
					if j+3+bc+2 <= len(bp) {
						vals := bp[j+3 : j+3+bc]
						fmt.Printf("  MODBUS resp @payload+14+%d: func=0x%02X bytecount=%d regs=%d\n", j, bp[j+1], bc, bc/2)
						for k := 0; k+1 < len(vals); k += 2 {
							v := binary.BigEndian.Uint16(vals[k : k+2])
							fmt.Printf("    reg[%04d] = 0x%04X (%d)\n", int(start)+k/2, v, v)
						}
						break
					}
				}
			}
		}
	}
}
