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
	sn, ok := antAveragedToBms26("AntBms 320 A/h (/dev/ttyUSB1)", bmsAveraged{CurrentA: 1, Soc: 50, CellsV: []float64{3.3}}, "2026-10-16T00:00:00+03:00")
	if !ok || sn.Serial != "AntBMS-DSP-320-00002" || sn.BmsType != "AntBMS-DSP" {
		t.Fatalf("ant averaged→bms26: %+v ok=%v", sn, ok)
	}
	en, ok := enbmsAveragedToBms26(enbmsAveraged{Soc: 60, CellsV: []float64{3.4}}, "2026-10-16T00:00:00+03:00")
	if !ok || en.Serial != "EMU-SN-1" || en.BmsType != "EnBMS-RS485" {
		t.Fatalf("enbms averaged→bms26: %+v ok=%v", en, ok)
	}
}
