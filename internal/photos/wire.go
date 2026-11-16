// Request layouts adapted from gotohp and gpmc, Copyright (c) 2024 xob0t.
// See THIRD_PARTY_NOTICES.md for the MIT license.
package photos

import (
	"errors"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"guludrive/core"
)

var errWire = errors.New("photos: invalid protobuf response")

type field struct {
	num   protowire.Number
	typ   protowire.Type
	bytes []byte
	value uint64
}

func fields(b []byte) ([]field, error) {
	out := make([]field, 0)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || num < 1 {
			return nil, errWire
		}
		b = b[n:]
		v := field{num: num, typ: typ}
		size := 0
		switch typ {
		case protowire.BytesType:
			v.bytes, size = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			v.value, size = protowire.ConsumeVarint(b)
		case protowire.Fixed32Type:
			_, size = protowire.ConsumeFixed32(b)
		case protowire.Fixed64Type:
			_, size = protowire.ConsumeFixed64(b)
		default:
			return nil, errWire
		}
		if size < 0 {
			return nil, errWire
		}
		b = b[size:]
		out = append(out, v)
		if len(out) > 100000 {
			return nil, errWire
		}
	}
	return out, nil
}
func bytesAt(b []byte, path ...protowire.Number) ([]byte, error) {
	for _, num := range path {
		fs, err := fields(b)
		if err != nil {
			return nil, err
		}
		var next []byte
		count := 0
		for _, f := range fs {
			if f.num == num {
				if f.typ != protowire.BytesType {
					return nil, errWire
				}
				next = f.bytes
				count++
			}
		}
		if count > 1 {
			return nil, errWire
		}
		if count == 0 {
			return nil, nil
		}
		b = next
	}
	return b, nil
}
func textAt(b []byte, path ...protowire.Number) (string, error) {
	s, err := bytesAt(b, path...)
	if err != nil {
		return "", err
	}
	if len(s) > 8192 || !utf8.Valid(s) {
		return "", errWire
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return "", errWire
		}
	}
	return string(s), nil
}

// rejectionDetails reads only google.rpc.Status machine fields. Status.message,
// FieldViolation.description, ErrorInfo.domain/metadata and unknown Any values
// are never retained. Bodies were already bounded and decoded by request.
func rejectionDetails(raw []byte, contentType string) *core.RemoteError {
	d := &core.RemoteError{BodyFormat: errorBodyFormat(raw, contentType)}
	if d.BodyFormat != "protobuf" {
		return d
	}
	fs, _ := fields(raw)
	codes, details := 0, 0
	for _, f := range fs {
		if f.num == 1 {
			codes++
			d.RPCCode = 0
			if codes == 1 && f.typ == protowire.VarintType && f.value <= 16 {
				d.RPCCode = int(f.value)
			}
		}
		if f.num != 3 || f.typ != protowire.BytesType || details >= 16 {
			continue
		}
		details++
		kind, _ := textAt(f.bytes, 1)
		value, err := bytesAt(f.bytes, 2)
		if err != nil {
			continue
		}
		switch kind {
		case "type.googleapis.com/google.rpc.ErrorInfo":
			reason, _ := textAt(value, 1)
			if safe := (&core.RemoteError{ErrorReason: reason}).Safe(); safe.ErrorReason != "" {
				d.ErrorReason = safe.ErrorReason
			}
		case "type.googleapis.com/google.rpc.BadRequest":
			violations, _ := fields(value)
			for _, v := range violations {
				if len(d.ViolationFields) >= 16 {
					break
				}
				if v.num != 1 || v.typ != protowire.BytesType {
					continue
				}
				field, _ := textAt(v.bytes, 1)
				if len(field) <= 96 {
					d.ViolationFields = append(d.ViolationFields, field)
				}
				reason, _ := textAt(v.bytes, 3)
				if safe := (&core.RemoteError{ErrorReason: reason}).Safe(); safe.ErrorReason != "" {
					d.ErrorReason = safe.ErrorReason
				}
			}
		}
	}
	return d.Safe()
}

func blob(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func str(n protowire.Number, s string) []byte { return blob(n, []byte(s)) }
func integer(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func join(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}
func device() []byte { return join(str(3, "Pixel XL"), str(4, "Google"), integer(5, 28)) }
func validateFinalize(b []byte) error {
	fs, err := fields(b)
	if err != nil {
		return err
	}
	v, o := 0, 0
	for _, f := range fs {
		switch f.num {
		case 1:
			if f.typ != protowire.VarintType || f.value != 2 {
				return errWire
			}
			v++
		case 2:
			if f.typ != protowire.BytesType || len(f.bytes) == 0 {
				return errWire
			}
			o++
		}
	}
	if v != 1 || o != 1 {
		return errWire
	}
	return nil
}
