// Package wincode contains small helpers for Agave's wincode wire format.
//
// The helpers here intentionally model only the default primitives Mithril
// needs today: fixed-width little-endian integers, u32 enum tags by convention,
// raw fixed-size byte fields, and byte vectors with u64 lengths.
package wincode
