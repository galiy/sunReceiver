package main

import "testing"

func setBits(a []int, base, n, val int) {
	for b := 0; b < n; b++ {
		if val&(1<<uint(b)) != 0 {
			a[(base+b)/8] |= 1 << uint((base+b)%8)
		}
	}
}

func TestDecodeBms26AlarmsAnt(t *testing.T) {
	a := make([]int, 19)
	setBits(a, 0, 4, 2) // charge code 2 → overvoltage protection
	setBits(a, 4, 4, 1) // discharge code 1 → норма
	out := decodeAntBms26Alarms(a)
	if len(out) != 1 || out[0] != "Charge: Overvoltage protection" {
		t.Fatalf("ant alarms=%v", out)
	}
	// system log: бит 0 → status=1
	a2 := make([]int, 19)
	setBits(a2, 12, 16, 1)
	out2 := decodeAntBms26Alarms(a2)
	if len(out2) != 1 {
		t.Fatalf("ant syslog alarms=%v", out2)
	}
}

func TestDecodeBms26AlarmsEn(t *testing.T) {
	sn := bms26Snapshot{CellCount: 2, TempCount: 1, BmsType: "EnBMS-RS485", AlarmBytes: make([]int, 19)}
	setBits(sn.AlarmBytes, 0, 4, 1) // ячейка1: высокое напряжение (warn)
	// Ext_Bit Byte1 (группа 1) бит0 → "Ячейка: превышение напряжения"
	sn.AlarmBytes[(100+1*8+0)/8] |= 1 << uint((100+1*8+0)%8)
	alarms, prot := decodeEnBms26Alarms(sn)
	got := map[string]bool{}
	for _, s := range alarms {
		got[s] = true
	}
	if !got["Ячейка 1: высокое напряжение (предупреждение)"] {
		t.Fatalf("нет тревоги ячейки: %v", alarms)
	}
	if !got["Ячейка: превышение напряжения"] {
		t.Fatalf("нет Ext_Bit тревоги: %v", alarms)
	}
	if len(prot) != 0 {
		t.Fatalf("защит быть не должно: %v", prot)
	}
}

func TestDecodeBms26AlarmsDispatch(t *testing.T) {
	if a, p := decodeBms26Alarms(bms26Snapshot{BmsType: "unknown", AlarmBytes: make([]int, 19)}); a != nil || p != nil {
		t.Fatal("неизвестный тип — без тревог")
	}
}

func TestBms26CellMasks(t *testing.T) {
	sn := bms26Snapshot{BmsType: "EnBMS-RS485", CellCount: 3, AlarmBytes: make([]int, 19)}
	setBits(sn.AlarmBytes, 0, 4, 2)   // ячейка1: protect (OVprot)
	setBits(sn.AlarmBytes, 2*4, 4, 1) // ячейка3: warn (HVwarn)
	w, p := bms26CellMasks(sn)
	if p != 0x1 {
		t.Fatalf("prot mask=%#x want 0x1", p)
	}
	if w != 0x4 {
		t.Fatalf("warn mask=%#x want 0x4", w)
	}
	// ANT: масок нет
	if w2, p2 := bms26CellMasks(bms26Snapshot{BmsType: "AntBMS-DSP", CellCount: 2, AlarmBytes: make([]int, 19)}); w2 != 0 || p2 != 0 {
		t.Fatal("ant не должен давать per-cell масок")
	}
}

func TestDecodeBms26ProtectionsEn(t *testing.T) {
	sn := bms26Snapshot{CellCount: 1, BmsType: "EnBMS-RS485", AlarmBytes: make([]int, 19)}
	setBits(sn.AlarmBytes, 1, 4, 1) // ячейка1, бит1 ниббла = OVprot
	alarms, prot := decodeEnBms26Alarms(sn)
	if len(alarms) != 0 {
		t.Fatalf("alarms=%v, want пусто", alarms)
	}
	if len(prot) != 1 || prot[0] != "Ячейка 1: защита от перенапряжения" {
		t.Fatalf("prot=%v", prot)
	}
}
