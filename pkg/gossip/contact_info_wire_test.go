package gossip

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// contactInfoV4WireFixtureHex is a captured CRDS ContactInfo (v4) record from a
// live cluster peer. Version fields are 0.4.4 with packed minor encoding and TLV
// extensions after the socket table.
const contactInfoV4WireFixtureHex = "0b0000007ab7efd48fd759236a63f4479797cd1d7b8ddbd6596b8de7e1b9a013e04fb6f69bafdccaf4334570e06b3656060044f9000404cc63f602cc41a40d0301000000007f0000010a00009a330a00a70b0900040100010c00010400010800010700010200f90603000100"

func TestDecodeContactInfoV4WireFixtureShredVersion(t *testing.T) {
	data, err := hex.DecodeString(contactInfoV4WireFixtureHex)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	record, err := decodeCrdsDataContactRecord(newDecoder(data))
	if err != nil {
		t.Fatalf("decode v4 contact record: %v", err)
	}
	if record.ShredVer != 63812 {
		t.Fatalf("shred version = %d, want 63812", record.ShredVer)
	}
	if !record.GossipAddr.ok {
		t.Fatalf("expected gossip addr")
	}
	idx := -1
	for i := 0; i+1 < len(data); i++ {
		if binary.LittleEndian.Uint16(data[i:]) == 63812 {
			idx = i
			break
		}
	}
	t.Logf("63812 found at byte offset %d in v4 fixture", idx)
}

func TestContactInfoV4WireFixtureShredFieldOffset(t *testing.T) {
	data, err := hex.DecodeString(contactInfoV4WireFixtureHex)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	d := newDecoder(data)
	if _, err := d.variant(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.read(32); err != nil {
		t.Fatal(err)
	}
	if _, err := d.varint(10); err != nil {
		t.Fatal(err)
	}
	shredNoOutset, err := d.u16()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("v4 fixture shred field without outset = %d", shredNoOutset)
}
