package main

import "testing"

func TestLegacyBms26Mapping(t *testing.T) {
	cacheBms26Snapshot(bms26Snapshot{Serial: "AntBMS-DSP-160-00001", Base: "AntBMS-DSP-160", BmsType: "AntBMS-DSP"})
	cacheBms26Snapshot(bms26Snapshot{Serial: "AntBMS-DSP-320-00002", Base: "AntBMS-DSP-320", BmsType: "AntBMS-DSP"})
	cacheBms26Snapshot(bms26Snapshot{Serial: "EMU-SN-1", BmsType: "EnBMS-RS485"})

	if s := legacyAntSerial(160); s != "AntBMS-DSP-160-00001" {
		t.Fatalf("ant160=%q", s)
	}
	if s := legacyEnbmsSerial(); s != "EMU-SN-1" {
		t.Fatalf("enbms=%q", s)
	}
}

func TestEnbmsPackAlarmRoundtrip(t *testing.T) {
	// сырые: ячейка1 warn(HV)=bit0; датчик1 protect(bit1); Ext Byte1 bit0 (группа1 warn).
	cellFlags := []byte{0x01}
	tempFlags := []byte{0x02}
	ext := []byte{0x00, 0x01}
	ab := enbmsPackAlarm(cellFlags, tempFlags, ext)
	sn := bms26Snapshot{BmsType: "EnBMS-RS485", CellCount: 1, TempCount: 1, AlarmBytes: ab}
	finalizeBms26(&sn)
	got := map[string]bool{}
	for _, s := range sn.Alarms {
		got[s] = true
	}
	if !got["Ячейка 1: высокое напряжение (предупреждение)"] {
		t.Fatalf("нет тревоги ячейки: %v", sn.Alarms)
	}
	if !got["Датчик 1: защита от перегрева"] {
		t.Fatalf("нет тревоги датчика: %v", sn.Alarms)
	}
	if !got["Ячейка: превышение напряжения"] {
		t.Fatalf("нет Ext_Bit тревоги: %v", sn.Alarms)
	}
	if sn.CellWarnMask != 0x1 || sn.TempProtMask != 0x1 {
		t.Fatalf("маски: cellWarn=%#x tempProt=%#x", sn.CellWarnMask, sn.TempProtMask)
	}
}

func TestLegacyAntAlarmBytes(t *testing.T) {
	ab := antAlarmBytesFromCodes(2, 1, 0)
	sn := bms26Snapshot{BmsType: "AntBMS-DSP", AlarmBytes: ab}
	finalizeBms26(&sn)
	if len(sn.Alarms) != 1 || sn.Alarms[0] != "Charge: Overvoltage protection" {
		t.Fatalf("ant alarms=%v", sn.Alarms)
	}
}

func TestAntCapParsers(t *testing.T) {
	if c, ok := antCapFromName("AntBms 160 A/h (/dev/ttyUSB0)"); !ok || c != 160 {
		t.Fatalf("name 160: %d %v", c, ok)
	}
	if c, ok := antCapFromName("AntBms 320 A/h (/dev/ttyUSB1)@/dev/ttyUSB1"); !ok || c != 320 {
		t.Fatalf("name 320: %d %v", c, ok)
	}
	if c, ok := antCapFromBase("AntBMS-DSP-160"); !ok || c != 160 {
		t.Fatalf("base 160: %d %v", c, ok)
	}
	if _, ok := antCapFromName("AntBMS-DSP-160"); ok {
		t.Fatal("name-парсер не должен матчить новый Base")
	}
}
