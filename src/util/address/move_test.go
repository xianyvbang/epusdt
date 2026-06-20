package addressutil

import (
	"strings"
	"testing"
)

func TestNormalizeMoveAddress(t *testing.T) {
	got, err := NormalizeMoveAddress(" A ")
	if err != nil {
		t.Fatalf("NormalizeMoveAddress: %v", err)
	}
	want := "0x000000000000000000000000000000000000000000000000000000000000000a"
	if got != want {
		t.Fatalf("address = %q, want %q", got, want)
	}

	got, err = NormalizeMoveAddress("0X" + "ABCDEF")
	if err != nil {
		t.Fatalf("NormalizeMoveAddress uppercase: %v", err)
	}
	want = "0x0000000000000000000000000000000000000000000000000000000000abcdef"
	if got != want {
		t.Fatalf("uppercase address = %q, want %q", got, want)
	}
}

func TestNormalizeMoveAddressRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "0xzz", "0x" + strings.Repeat("1", 65)} {
		if _, err := NormalizeMoveAddress(input); err == nil {
			t.Fatalf("NormalizeMoveAddress(%q) succeeded, want error", input)
		}
	}
}

func TestNormalizeMoveAssetIDNormalizesLeadingAddress(t *testing.T) {
	got := NormalizeMoveAssetID("0X1::FUNGIBLE_ASSET::METADATA")
	want := "0x0000000000000000000000000000000000000000000000000000000000000001::fungible_asset::metadata"
	if got != want {
		t.Fatalf("asset id = %q, want %q", got, want)
	}
}
