package solarman

import (
	"fmt"
	"net"
	"time"
)

type Client struct {
	Address    string
	DeviceSN   uint32
	Timeout    time.Duration
	IdleWindow time.Duration
	SequenceID uint16
}

func NewClient(address string, deviceSN uint32, timeout time.Duration) *Client {
	return &Client{
		Address:    address,
		DeviceSN:   deviceSN,
		Timeout:    timeout,
		IdleWindow: 4 * time.Second,
	}
}

// Exchange — одно TCP-соединение, один запрос, сбор всех кадров ответа.
// Чтение идёт до «тишины» (IdleWindow без новых байтов) или общего Timeout.
func (c *Client) Exchange(req []byte) ([]Frame, []byte, error) {
	conn, err := net.DialTimeout("tcp", c.Address, c.Timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return nil, nil, err
	}
	if _, err := conn.Write(req); err != nil {
		return nil, nil, fmt.Errorf("write: %w", err)
	}
	idle := c.IdleWindow
	if idle <= 0 {
		idle = 4 * time.Second
	}

	var raw []byte
	buf := make([]byte, 1024)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
			break
		}
		n, _ := conn.Read(buf)
		if n > 0 {
			raw = append(raw, buf[:n]...)
			if len(raw) > 4096 {
				break
			}
			continue
		}
		// idle таймаут или ошибка соединения — конец ответа
		break
	}
	if len(raw) == 0 {
		return nil, raw, fmt.Errorf("no data from %s", c.Address)
	}
	return SplitFrames(raw), raw, nil
}

// ReadRegisters — запрос чтения startReg..startReg+regCount-1.
// Возвращает распарсенные PDU (может быть несколько) и сырой ответ.
func (c *Client) ReadRegisters(startReg, regCount uint16) ([]ModbusPDU, []Frame, error) {
	req := BuildReadFrame(c.DeviceSN, startReg, regCount)
	frames, _, err := c.Exchange(req)
	if err != nil {
		return nil, nil, err
	}
	var pdus []ModbusPDU
	for _, f := range frames {
		pdus = append(pdus, ParseModbusPDU(f.Payload)...)
	}
	return pdus, frames, nil
}
