package main

import "testing"

// TestDecodeDeyeFaults_GridLoss фиксирует реальное событие 2026-10-02: пропадание
// сети у трёх grid-tie Deye давало regs 0x65-0x6A = 0x...0004 0000, т.е. 0x69.2.
// Карта R103-R106 = 0x67-0x6A (младшее слово — 0x67), бит N-1 = F(N) → F35.
func TestDecodeDeyeFaults_GridLoss(t *testing.T) {
	regs := map[uint16]uint16{0x69: 0x0004}
	got := decodeDeyeFaults(regs)
	if len(got) != 1 || got[0] != "F35: нет сети (AC_NoUtility)" {
		t.Fatalf("decodeDeyeFaults = %v, ожидалось [F35: нет сети (AC_NoUtility)]", got)
	}
}

// TestDecodeDeyeWarnings: предупреждения — регистры 0x65/0x66, бит N-1 = W(N).
func TestDecodeDeyeWarnings(t *testing.T) {
	got := decodeDeyeWarnings(map[uint16]uint16{0x65: 0x0002, 0x66: 0x0001})
	want := []string{"W2: неисправность вентилятора", "W17: небаланс токов фаз"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("decodeDeyeWarnings = %v, ожидалось %v", got, want)
	}
	if decodeDeyeWarnings(map[uint16]uint16{}) != nil {
		t.Fatal("без предупреждений ожидался nil")
	}
}

func TestDecodeDeyeFaults_NoFault(t *testing.T) {
	if got := decodeDeyeFaults(map[uint16]uint16{}); got != nil {
		t.Fatalf("без аварий ожидался nil, получено %v", got)
	}
}
