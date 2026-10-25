package versenbelegd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"github.com/beepbite/backend/internal/channel"
)

// SignatureHeader is the HTTP header carrying the hex HMAC-SHA256 of the raw
// body, prefixed with "sha256=" — the same shape as GitHub and Meta, so a
// Worker that already signs webhooks does not need a new convention.
const SignatureHeader = "X-Vers-En-Belegd-Signature"

// VerifySignature checks header against HMAC-SHA256(secret, body).
//
// An empty secret is channel.ErrNotConfigured, not a bad signature: the
// operator has not turned the rail on, which is a different event from a
// caller who has the wrong key. Comparison is constant-time.
func VerifySignature(secret, header string, body []byte) error {
	if strings.TrimSpace(secret) == "" {
		return channel.ErrNotConfigured
	}
	const prefix = "sha256="
	hexPart, ok := strings.CutPrefix(strings.TrimSpace(header), prefix)
	if !ok || hexPart == "" {
		return errBadSignature
	}
	got, err := hex.DecodeString(hexPart)
	if err != nil {
		return errBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errBadSignature
	}
	return nil
}

// errBadSignature is private so callers match it with errors.Is via the
// exported alias below, and so a wrapped form still identifies the failure.
var errBadSignature = &signatureError{}

// ErrBadSignature is returned when the header is missing, malformed, or does
// not match the body. The handler answers 401. The error text is deliberately
// the same in every case so a caller cannot probe which check failed.
var ErrBadSignature error = errBadSignature

type signatureError struct{}

func (signatureError) Error() string { return "vers-en-belegd: invalid signature" }

// Sign is the counterpart of VerifySignature, for the ordering site and for
// tests. It returns the header value, including the "sha256=" prefix.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ResolveSKU maps a site product code onto a BeepBite items.sku.
//
// A missing entry, or a nil map, means the site sku is already the BeepBite
// sku. That is the trial default: set the same code on both menus, and only
// fill the map where they disagree.
func ResolveSKU(sku string, m map[string]string) string {
	sku = strings.TrimSpace(sku)
	if m == nil {
		return sku
	}
	if mapped, ok := m[sku]; ok {
		if v := strings.TrimSpace(mapped); v != "" {
			return v
		}
	}
	return sku
}

// LoadSKUMap reads a JSON object of site sku → BeepBite sku.
// An empty path returns a nil map and no error, which ResolveSKU treats as
// identity. A path that cannot be read, or a file that is not an object of
// strings, is an error: a broken map must not silently accept orders against
// the wrong products.
func LoadSKUMap(path string) (map[string]string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
