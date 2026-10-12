package extproc

import (
	"encoding/binary"
	"errors"
	"math"
)

// A minimal protocol buffers wire-format reader and writer: enough for the
// handful of ext_proc messages the endpoint picker reads and writes, so the
// router keeps depending on nothing but the Go standard library.

const (
	wireVarint = 0
	wireI64    = 1
	wireBytes  = 2
	wireI32    = 5
)

var errTruncated = errors.New("protobuf: truncated message")

// fields calls fn for each field in msg. For varint fields v is the value;
// for length-delimited fields b is the payload; fixed-width fields are
// passed in v. It stops at the first error.
func fields(msg []byte, fn func(num int, wire int, v uint64, b []byte) error) error {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 {
			return errTruncated
		}
		msg = msg[n:]
		num, wire := int(key>>3), int(key&7)
		var v uint64
		var b []byte
		switch wire {
		case wireVarint:
			v, n = binary.Uvarint(msg)
			if n <= 0 {
				return errTruncated
			}
			msg = msg[n:]
		case wireI64:
			if len(msg) < 8 {
				return errTruncated
			}
			v, msg = binary.LittleEndian.Uint64(msg), msg[8:]
		case wireBytes:
			l, n := binary.Uvarint(msg)
			if n <= 0 || l > uint64(len(msg)-n) {
				return errTruncated
			}
			b, msg = msg[n:n+int(l)], msg[n+int(l):]
		case wireI32:
			if len(msg) < 4 {
				return errTruncated
			}
			v, msg = uint64(binary.LittleEndian.Uint32(msg)), msg[4:]
		default:
			return errors.New("protobuf: unsupported wire type")
		}
		if err := fn(num, wire, v, b); err != nil {
			return err
		}
	}
	return nil
}

// enc builds a message.
type enc []byte

func (e enc) key(num, wire int) enc { return binary.AppendUvarint(e, uint64(num)<<3|uint64(wire)) }

func (e enc) varint(num int, v uint64) enc {
	if v == 0 {
		return e // proto3 default: omitted
	}
	return binary.AppendUvarint(e.key(num, wireVarint), v)
}

func (e enc) boolean(num int, v bool) enc {
	if !v {
		return e
	}
	return e.varint(num, 1)
}

func (e enc) bytes(num int, b []byte) enc {
	e = binary.AppendUvarint(e.key(num, wireBytes), uint64(len(b)))
	return append(e, b...)
}

func (e enc) str(num int, s string) enc { return e.bytes(num, []byte(s)) }

// msg embeds a sub-message (always written, so an empty one still selects
// its oneof case).
func (e enc) msg(num int, m enc) enc { return e.bytes(num, m) }

func (e enc) double(num int, f float64) enc {
	return binary.LittleEndian.AppendUint64(e.key(num, wireI64), math.Float64bits(f))
}
