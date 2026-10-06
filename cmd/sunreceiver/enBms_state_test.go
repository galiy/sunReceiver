package main

import "testing"

// buildTeleState собирает синтетический INFO блока TeleState (0x44, 49 байт):
// header(2) + cellCount(1) + cellFlags[n] + tempCount(1) + tempFlags[tn] +
// GB(2) + extCount(1) + ext[extCount-1] + mode(1).
func buildTeleState(cells, temps int, ext []byte, mode byte) []byte {
	p := []byte{0x00, 0x00, byte(cells)}
	p = append(p, make([]byte, cells)...)
	p = append(p, byte(temps))
	p = append(p, make([]byte, temps)...)
	p = append(p, 0x00, 0x00) // GB ток/напряжение
	p = append(p, byte(len(ext)+1))
	p = append(p, ext...)
	p = append(p, mode)
	return p
}

func TestParseEnBmsState(t *testing.T) {
	ext := make([]byte, 19)
	ext[6] = 0x03 // группа 6: разрядный(0) + зарядный(1) ключи включены
	ext[7] = 0x08 // группа 7: балансировка ячейки 4

	p := buildTeleState(16, 6, ext, 0x01) // режим «Разряд»
	if len(p) != 49 {
		t.Fatalf("длина TeleState = %d, ожидалось 49", len(p))
	}
	st, err := parseEnBmsState(p)
	if err != nil {
		t.Fatalf("parseEnBmsState: %v", err)
	}
	if st.Mode != "Разряд" {
		t.Errorf("Mode = %q, ожидалось «Разряд»", st.Mode)
	}
	if len(st.Alarms) != 0 {
		t.Errorf("Alarms = %v, ожидалось пусто", st.Alarms)
	}
	if len(st.Keys) != 2 {
		t.Errorf("Keys = %v, ожидалось 2 записи", st.Keys)
	}
	if len(st.Balance) != 1 || st.Balance[0] != "Ячейка 4" {
		t.Errorf("Balance = %v, ожидалось [Ячейка 4]", st.Balance)
	}
	if len(st.BalanceCells) != 1 || st.BalanceCells[0] != 4 {
		t.Errorf("BalanceCells = %v, ожидалось [4]", st.BalanceCells)
	}
	if m := enbmsBalanceMask(st.BalanceCells); m != 1<<3 {
		t.Errorf("enbmsBalanceMask = %#x, ожидалось 0x8", m)
	}
}

func TestParseEnBmsStateAlarms(t *testing.T) {
	// Ячейка 1 — защита от перенапряжения (бит 1); Ext_Bit группа 1, бит 1 — защита.
	ext := make([]byte, 19)
	ext[1] = 0x02
	p := buildTeleState(2, 2, ext, 0x00)
	p[3] = 0x02 // cellFlags[0]
	st, err := parseEnBmsState(p)
	if err != nil {
		t.Fatalf("parseEnBmsState: %v", err)
	}
	if len(st.Alarms) != 2 {
		t.Fatalf("Alarms = %v, ожидалось 2", st.Alarms)
	}
}

func TestParseEnBmsStateShort(t *testing.T) {
	if _, err := parseEnBmsState([]byte{0x00}); err == nil {
		t.Fatal("ожидалась ошибка на коротком кадре")
	}
}
