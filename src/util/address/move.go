package addressutil

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// NormalizeMoveAddress returns the canonical 0x-prefixed, 64-hex-character
// account address used by Aptos.
func NormalizeMoveAddress(input string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(input))
	raw = strings.TrimPrefix(raw, "0x")
	if raw == "" {
		return "", fmt.Errorf("move address is empty")
	}
	if len(raw) > 64 {
		return "", fmt.Errorf("move address is too long")
	}
	if _, err := hex.DecodeString(padEvenHex(raw)); err != nil {
		return "", fmt.Errorf("invalid move address")
	}
	return "0x" + strings.Repeat("0", 64-len(raw)) + raw, nil
}

func padEvenHex(raw string) string {
	if len(raw)%2 == 0 {
		return raw
	}
	return "0" + raw
}

// NormalizeMoveAssetID canonicalizes Move asset identifiers used in
// chain_tokens.contract_address for Aptos. A plain address is normalized to
// 64 hex chars; structured types keep module/name suffixes after normalizing
// the leading address.
func NormalizeMoveAssetID(input string) string {
	raw := strings.ToLower(strings.TrimSpace(input))
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "::")
	if len(parts) == 1 {
		if addr, err := NormalizeMoveAddress(raw); err == nil {
			return addr
		}
		return raw
	}
	if addr, err := NormalizeMoveAddress(parts[0]); err == nil {
		parts[0] = addr
	}
	return strings.Join(parts, "::")
}

func MoveAssetMatches(observed, configured string) bool {
	observed = NormalizeMoveAssetID(observed)
	configured = NormalizeMoveAssetID(configured)
	if configured == "" {
		return observed == ""
	}
	if observed == configured {
		return true
	}
	// Aptos FA event payloads may expose only the metadata address while other
	// payloads expose a type string containing the same address.
	return strings.Contains(observed, configured)
}
