package main

import "testing"

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
	if c, ok := antCapFromBase("AntBMS-DSP-320"); !ok || c != 320 {
		t.Fatalf("base 320: %d %v", c, ok)
	}
	if _, ok := antCapFromName("AntBMS-DSP-160"); ok {
		t.Fatal("name-парсер не должен матчить новый Base")
	}
}
