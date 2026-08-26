package sysinfo

import (
	"path"
)

// Card is a representation of a system GPU
type Card struct {
	Index     int    // Internal Kernel index
	Path      string // Path to the drm card
	Device    string // Path to the PCI device
	Driver    string // Base driver name
	Embedded  bool   // Integrated display
	VendorID  string
	ProductID string

	// Metadata added in top-level implementation
	Vendor  string
	Product string
}

func (c Card) String() string {
	return c.Vendor + " " + c.Product
}

func (c *Card) Addr() string {
	return path.Base(c.Device)
}

// intelHaswellIDs are the PCI device IDs of Intel's 4th Gen Core
// ("Haswell", gfx gen 7.5) integrated GPUs, taken from the Linux kernel's
// i915 driver (INTEL_HSW_IDS). Mesa's ANV Vulkan driver only offers
// partial support for this generation and logs a "Haswell Vulkan support
// is incomplete" warning on it, which in turn causes Studio's D3D-over-
// Vulkan (DXVK) and native Vulkan renderers to be unreliable.
var intelHaswellIDs = map[string]bool{
	"0402": true, "0412": true, "0422": true,
	"040a": true, "041a": true, "042a": true,
	"040b": true, "041b": true, "042b": true,
	"040e": true, "041e": true, "042e": true,
	"0c02": true, "0c12": true, "0c22": true,
	"0c0a": true, "0c1a": true, "0c2a": true,
	"0c0b": true, "0c1b": true, "0c2b": true,
	"0c0e": true, "0c1e": true, "0c2e": true,
	"0a02": true, "0a12": true, "0a22": true,
	"0a0a": true, "0a1a": true, "0a2a": true,
	"0a0b": true, "0a1b": true, "0a2b": true,
	"0d02": true, "0d12": true, "0d22": true,
	"0d0a": true, "0d1a": true, "0d2a": true,
	"0d0b": true, "0d1b": true, "0d2b": true,
	"0d0e": true, "0d1e": true, "0d2e": true,
	"0406": true, "0416": true, "0426": true,
	"0c06": true, "0c16": true, "0c26": true,
	"0a06": true, "0a16": true, "0a26": true,
	"0a0e": true, "0a1e": true, "0a2e": true,
	"0d06": true, "0d16": true, "0d26": true,
}

// IncompleteVulkan reports whether this card is known to have
// incomplete/unreliable Vulkan support, and should therefore not be
// defaulted to a Vulkan-based renderer.
func (c *Card) IncompleteVulkan() bool {
	return c.VendorID == "8086" && intelHaswellIDs[c.ProductID]
}
