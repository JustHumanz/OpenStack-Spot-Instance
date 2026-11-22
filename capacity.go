package main

import "math"

type Inventory struct {
	Total           int     `json:"total"`
	Reserved        int     `json:"reserved"`
	AllocationRatio float64 `json:"allocation_ratio"`
	MinUnit         int     `json:"min_unit"`
	MaxUnit         int     `json:"max_unit"`
	StepSize        int     `json:"step_size"`
}

func (inventory Inventory) Available(used int) int {
	return int(math.Floor(float64(inventory.Total-inventory.Reserved)*inventory.AllocationRatio)) - used
}

func (inventory Inventory) Fits(used, requested int) bool {
	return requested > 0 && inventory.AllocationRatio > 0 && inventory.Total >= inventory.Reserved &&
		requested >= inventory.MinUnit && (inventory.MaxUnit == 0 || requested <= inventory.MaxUnit) &&
		(inventory.StepSize <= 1 || requested%inventory.StepSize == 0) && inventory.Available(used) >= requested
}
