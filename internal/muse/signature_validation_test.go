package muse

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testMetaReasoningSignature builds a synthetic Meta Muse envelope with the
// observed fixed header and a power-of-two payload bucket. The body bytes are
// deterministic filler; recognition is a transport-shape check, so no real
// captured value is needed (or permitted) here.
func testMetaReasoningSignature(payloadLen int) string {
	return base64.RawURLEncoding.EncodeToString(testMetaReasoningSignatureBytes(payloadLen, nil))
}

func testMetaReasoningSignatureBytes(payloadLen int, mutate func([]byte)) []byte {
	decoded := make([]byte, metaReasoningSignatureOverheadLen+payloadLen)
	copy(decoded, metaReasoningSignatureHeader)
	for i := len(metaReasoningSignatureHeader); i < len(decoded); i++ {
		decoded[i] = byte(i*31 + 7)
	}
	if mutate != nil {
		mutate(decoded)
	}
	return decoded
}

// testMetaRouterSuffix builds a synthetic router suffix in the observed
// OpenRouter shape: a single run of 67 alphanumeric characters.
func testMetaRouterSuffix() string {
	return strings.Repeat("Ab3", 23)[:67]
}

func TestInspectMetaReasoningSignature_ValidBuckets(t *testing.T) {
	for _, tc := range []struct {
		payload  int
		exponent int
	}{
		{1024, 0},
		{2048, 1},
		{4096, 2},
		{32768, 5},
	} {
		sig := testMetaReasoningSignature(tc.payload)
		info, err := InspectMetaReasoningSignature(sig)
		if err != nil {
			t.Fatalf("payload %d: InspectMetaReasoningSignature error = %v", tc.payload, err)
		}
		if info.DecodedLen != tc.payload+metaReasoningSignatureOverheadLen || info.PayloadLen != tc.payload || info.BucketExponent != tc.exponent {
			t.Fatalf("payload %d: info = %+v", tc.payload, info)
		}
		if !IsValidMetaReasoningSignature(sig) {
			t.Fatalf("payload %d: IsValidMetaReasoningSignature = false", tc.payload)
		}
	}
}

func TestInspectMetaReasoningSignature_RejectsMalformed(t *testing.T) {
	valid := testMetaReasoningSignature(1024)
	encode := func(payloadLen int, mutate func([]byte)) string {
		return base64.RawURLEncoding.EncodeToString(testMetaReasoningSignatureBytes(payloadLen, mutate))
	}
	nonCanonical := func() string {
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		last := strings.IndexByte(alphabet, valid[len(valid)-1])
		for i := 0; i < len(alphabet); i++ {
			if i != last && i&0x30 == last&0x30 {
				return valid[:len(valid)-1] + string(alphabet[i])
			}
		}
		return valid
	}()
	cases := map[string]string{
		"empty":               "",
		"mock prefix only":    "Q-PaDg-mock-muse-spark-encrypted-reasoning-payload",
		"wrong literal start": "R" + valid[1:],
		"header byte flipped": encode(1024, func(b []byte) { b[2] ^= 0xff }),
		"version byte zero":   encode(1024, func(b []byte) { b[4] = 0x00 }),
		"payload below min":   encode(512, nil),
		"payload not power2":  encode(1025, nil),
		"payload off by one":  encode(2047, nil),
		"padded encoding":     valid + "=",
		"standard base64 +":   valid[:100] + "+" + valid[101:],
		"embedded whitespace": valid[:100] + " " + valid[100:],
		"length mod4 is 1":    valid[:len(valid)-1],
		"non-canonical bits":  nonCanonical,
	}
	for name, sig := range cases {
		if _, err := InspectMetaReasoningSignature(sig); err == nil {
			t.Errorf("%s: InspectMetaReasoningSignature unexpectedly succeeded", name)
		}
	}
}

func TestInspectMetaReasoningSignature_RouterSuffixed(t *testing.T) {
	native := testMetaReasoningSignature(2048)
	suffix := testMetaRouterSuffix()
	routed := native + "." + suffix

	info, err := InspectMetaReasoningSignature(routed)
	if err != nil {
		t.Fatalf("InspectMetaReasoningSignature(routed) error = %v", err)
	}
	if info.RouterSuffix != suffix {
		t.Fatalf("RouterSuffix = %q, want %q", info.RouterSuffix, suffix)
	}
	nativeInfo, err := InspectMetaReasoningSignature(native)
	if err != nil {
		t.Fatalf("InspectMetaReasoningSignature(native) error = %v", err)
	}
	if nativeInfo.RouterSuffix != "" {
		t.Fatalf("native RouterSuffix = %q, want empty", nativeInfo.RouterSuffix)
	}
	if info.DecodedLen != nativeInfo.DecodedLen || info.PayloadLen != nativeInfo.PayloadLen || info.BucketExponent != nativeInfo.BucketExponent {
		t.Fatalf("routed info = %+v, want same envelope fields as native %+v", info, nativeInfo)
	}
}

func TestInspectMetaReasoningSignature_RejectsMalformedRouterSuffix(t *testing.T) {
	native := testMetaReasoningSignature(1024)
	suffix := testMetaRouterSuffix()
	encode := func(payloadLen int, mutate func([]byte)) string {
		return base64.RawURLEncoding.EncodeToString(testMetaReasoningSignatureBytes(payloadLen, mutate))
	}
	cases := map[string]string{
		"trailing dot":           native + ".",
		"oversized suffix":       native + "." + strings.Repeat("a", maxMetaRouterSuffixLen+1),
		"suffix with dash":       native + "." + suffix[:60] + "-" + suffix[61:],
		"suffix with underscore": native + "." + suffix[:60] + "_" + suffix[61:],
		"suffix with padding":    native + "." + suffix[:60] + "=" + suffix[61:],
		"suffix with plus":       native + "." + suffix[:60] + "+" + suffix[61:],
		"suffix with slash":      native + "." + suffix[:60] + "/" + suffix[61:],
		"suffix with space":      native + "." + suffix[:60] + " " + suffix[61:],
		"second dot":             native + "." + suffix[:60] + "." + suffix[61:],
		"bad native header":      encode(1024, func(b []byte) { b[2] ^= 0xff }) + "." + suffix,
		"non-bucket native":      encode(1025, nil) + "." + suffix,
		"suffix only":            "." + suffix,
	}
	for name, sig := range cases {
		if _, err := InspectMetaReasoningSignature(sig); err == nil {
			t.Errorf("%s: InspectMetaReasoningSignature unexpectedly succeeded", name)
		}
	}
}
