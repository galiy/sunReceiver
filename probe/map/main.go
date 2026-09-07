// Проба чтения ячеек МАП Титанатор («КЭС») по Modbus TCP.
//
// Команда:
//   go run ./probe/map <host> <unit> <read_spec> [read_spec ...]
//     host       — IP МАП (порт 502 добавляется сам)
//     unit       — Modbus-адрес устройства (обычно 1)
//     read_spec  — <start_hex>:<count>  например «405:2», «530:40», «400:20»
//
// МАП хранит ячейки побайтно и отвечает на func 03 (чтение регистров). Каждый
// регистр = слово из 2 байт: значение ячейки по адресу A лежит в СТАРШЕМ байте
// слова, младший байт — значение следующей ячейки A+1. Диагностика печатает
// hex-dump ответа и каждую ячейку (адрес, значение).
//
// Гейт отвечает быстро, но на ПЕРВОМ запросе после простоя может занять время;
// повторные чтения на том же соединении мгновенные. Проба открывает ОДНО
// соединение и переиспользует его (как mapread.py), поэтому первый проход может
// занять несколько секунд.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

var timeout = flag.Duration("t", 5*time.Second, "таймаут чтения ответа на каждом запросе")

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: go run ./probe/map <host> <unit> <read_spec> [read_spec ...]\n")
		fmt.Fprintf(os.Stderr, "  read_spec: <start_hex>:<count>  (например 405:2, 530:40, 400:20)\n")
		os.Exit(2)
	}
	host := args[0]
	if _, _, e := net.SplitHostPort(host); e != nil {
		host = host + ":502"
	}
	unit, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "unit: %v\n", err)
		os.Exit(2)
	}

	conn, err := net.DialTimeout("tcp", host, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", host, err)
		os.Exit(1)
	}
	defer conn.Close()

	var txn uint16
	for _, spec := range args[2:] {
		var s, c int
		if _, err := fmt.Sscanf(spec, "%x:%x", &s, &c); err != nil {
			fmt.Fprintf(os.Stderr, "bad spec %q: %v\n", spec, err)
			continue
		}
		txn++
		pdu := []byte{0x03, byte(s >> 8), byte(s), byte(c >> 8), byte(c)}
		mbapLen := 1 + len(pdu) // Modbus TCP Length = unit id + PDU
		req := make([]byte, 0, mbapLen+len(pdu))
		req = append(req, byte(txn>>8), byte(txn), 0, 0, byte(mbapLen>>8), byte(mbapLen), byte(unit))
		req = append(req, pdu...)
		if err := conn.SetDeadline(time.Now().Add(*timeout)); err != nil {
			fmt.Fprintf(os.Stderr, "set deadline: %v\n", err)
			return
		}
		if _, err := conn.Write(req); err != nil {
			fmt.Printf("read 0x%03X count %d: write: %v\n", s, c, err)
			return
		}
		buf := make([]byte, 2048)
		n, err := conn.Read(buf)
		if err != nil {
			fmt.Printf("read 0x%03X count %d: ERROR %v\n", s, c, err)
			return
		}
		raw := buf[:n]
		fmt.Printf("read 0x%03X count %d -> %d bytes:\n%s\n", s, c, len(raw), hex.Dump(raw))
		if len(raw) < 9 || raw[7]&0x80 != 0 {
			continue
		}
		bc := int(raw[8])
		for i := 0; i+1 < bc; i += 2 {
			addr := s + i
			hi := raw[9+i]
			lo := raw[9+i+1]
			fmt.Printf("  cell 0x%03X = %3d (0x%02X) | cell 0x%03X = %3d (0x%02X)\n",
				addr, hi, hi, addr+1, lo, lo)
		}
	}
}