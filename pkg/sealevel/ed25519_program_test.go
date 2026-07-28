package sealevel

import (
	"testing"

	"github.com/oasisprotocol/curve25519-voi/primitives/ed25519"
)

// The precompile predicate is expressed as a struct literal, and Go zeroes every
// field a literal omits. That makes "forgot a field" indistinguishable from
// "deliberately false" at the call site, and the two differ here: omitting
// AllowNonCanonicalA rejects public keys the reference accepts.
//
// This pins each field individually so a regression names the field it broke
// rather than failing on an opaque struct comparison.
func TestEd25519PrecompileStrictVerifyOptions(t *testing.T) {
	opts := ed25519PrecompileStrictVerifyOptions

	for _, tc := range []struct {
		field string
		got   bool
		want  bool
		why   string
	}{
		{
			field: "AllowSmallOrderA",
			got:   opts.AllowSmallOrderA,
			want:  false,
			why:   "strict verification rejects a small-order public key",
		},
		{
			field: "AllowSmallOrderR",
			got:   opts.AllowSmallOrderR,
			want:  false,
			why:   "strict verification rejects a small-order R",
		},
		{
			field: "AllowNonCanonicalA",
			got:   opts.AllowNonCanonicalA,
			want:  true,
			why: "a non-canonical A is accepted and its original bytes are hashed; " +
				"the zero value would reject it, which is stricter than the reference",
		},
		{
			field: "CofactorlessVerify",
			got:   opts.CofactorlessVerify,
			want:  true,
			why:   "the reference uses the cofactorless equation",
		},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v: %s", tc.field, tc.got, tc.want, tc.why)
		}
	}
}

// voi does not return a verdict for AllowNonCanonicalR + CofactorlessVerify --
// it panics. Setting that field would therefore crash the node on the first
// precompile instruction rather than merely loosening a check, so this pins both
// that the field stays unset and the reason it must.
func TestEd25519NonCanonicalRIsIncompatibleWithCofactorless(t *testing.T) {
	if !ed25519PrecompileStrictVerifyOptions.CofactorlessVerify {
		t.Fatal("precondition: options must use cofactorless verification")
	}
	if ed25519PrecompileStrictVerifyOptions.AllowNonCanonicalR {
		t.Fatal("AllowNonCanonicalR must stay unset while CofactorlessVerify is set")
	}

	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		opts := ed25519.Options{
			Verify: &ed25519.VerifyOptions{
				AllowNonCanonicalR: true,
				CofactorlessVerify: true,
			},
		}
		ed25519.VerifyWithOptions(
			make([]byte, ed25519.PublicKeySize),
			[]byte("m"),
			make([]byte, ed25519.SignatureSize),
			&opts,
		)
		return false
	}()

	if !panicked {
		t.Fatal("voi no longer panics on AllowNonCanonicalR + CofactorlessVerify; " +
			"re-check whether that combination is now usable")
	}
}
