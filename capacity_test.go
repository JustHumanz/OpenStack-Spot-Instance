package main

import "testing"

func TestInventoryFits(t *testing.T) {
	memory := Inventory{Total: 65536, Reserved: 2048, AllocationRatio: 1, MinUnit: 1, MaxUnit: 65536, StepSize: 1}
	if !memory.Fits(61440, 2048) || memory.Fits(61440, 2049) {
		t.Fatal("reserved memory must not be allocated")
	}
	cpu := Inventory{Total: 16, Reserved: 2, AllocationRatio: 4, MinUnit: 1, MaxUnit: 16, StepSize: 1}
	if !cpu.Fits(52, 4) || cpu.Fits(53, 4) {
		t.Fatal("CPU capacity must apply reservation and allocation ratio")
	}
}
