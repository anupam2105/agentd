package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// canonicalJSON re-encodes v with sorted object keys and preserved number
// literals, so semantically equal requests hash equally.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("request is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("request has trailing data")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func requestHash(kind string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}
