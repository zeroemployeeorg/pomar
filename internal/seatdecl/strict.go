package seatdecl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// strictDecode reads exactly one JSON value into v, refusing unknown
// fields, trailing data, and a key given twice in one object. encoding/json
// keeps the last of two equal keys, and matches field names without regard
// to case, so a document whose bytes show one value could validate as
// another; its sha256, which is the document's identity, covers both. Keys
// are therefore compared case-folded, and a key outside ASCII is refused:
// no field or map key of these schemas needs one.
func strictDecode(raw []byte, v any, what string) error {
	if err := uniqueKeys(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%s: trailing data", what)
	}
	return nil
}

func uniqueKeys(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, _ := key.(string)
			for i := 0; i < len(name); i++ {
				if name[i] >= 0x80 {
					return fmt.Errorf("key %q is not ASCII", name)
				}
			}
			folded := strings.ToLower(name)
			if seen[folded] {
				return fmt.Errorf("key %q is given twice", name)
			}
			seen[folded] = true
			if err := uniqueKeys(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueKeys(d, depth+1); err != nil {
				return err
			}
		}
	}
	_, err = d.Token()
	return err
}
