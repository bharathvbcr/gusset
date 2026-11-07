package uniffi_bindgen_go

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

var (
	// ErrBufferUnderflow is returned when reading past the end of a RustBuffer.
	ErrBufferUnderflow = errors.New("uniffi: buffer underflow reading serialized data")
	// ErrStringTooLong is returned when WriteString or WriteBytes is given a value
	// whose length does not fit the signed 32-bit length prefix (over math.MaxInt32
	// bytes). Nothing is written in that case.
	ErrStringTooLong = errors.New("uniffi: string length exceeds maximum allowable capacity")
	// ErrInvalidEncoding is returned by ReadString when the bytes are not valid
	// UTF-8. The Rust engine rejects the same input.
	ErrInvalidEncoding = errors.New("uniffi: invalid UTF-8 string encoding")
)

// RustBufferWriter encodes typed primitives into the big-endian wire format expected by UniFFI.
type RustBufferWriter struct {
	buf []byte
}

// NewWriter returns a new RustBufferWriter with initial capacity.
func NewWriter(initialCapacity int) *RustBufferWriter {
	return &RustBufferWriter{
		buf: make([]byte, 0, initialCapacity),
	}
}

// Bytes returns the serialized byte slice.
func (w *RustBufferWriter) Bytes() []byte {
	return w.buf
}

// WriteInt8 writes a signed 8-bit integer.
func (w *RustBufferWriter) WriteInt8(v int8) {
	w.buf = append(w.buf, byte(v))
}

// WriteUint8 writes an unsigned 8-bit integer.
func (w *RustBufferWriter) WriteUint8(v uint8) {
	w.buf = append(w.buf, v)
}

// WriteInt16 writes a signed 16-bit integer in big-endian order.
func (w *RustBufferWriter) WriteInt16(v int16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(v))
	w.buf = append(w.buf, b[:]...)
}

// WriteInt32 writes a signed 32-bit integer in big-endian order.
func (w *RustBufferWriter) WriteInt32(v int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	w.buf = append(w.buf, b[:]...)
}

// WriteUint32 writes an unsigned 32-bit integer in big-endian order.
func (w *RustBufferWriter) WriteUint32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteInt64 writes a signed 64-bit integer in big-endian order.
func (w *RustBufferWriter) WriteInt64(v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	w.buf = append(w.buf, b[:]...)
}

// WriteFloat64 writes a 64-bit IEEE float in big-endian order.
func (w *RustBufferWriter) WriteFloat64(v float64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(v))
	w.buf = append(w.buf, b[:]...)
}

// WriteBool writes a boolean (1 for true, 0 for false).
func (w *RustBufferWriter) WriteBool(v bool) {
	if v {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}

// lengthPrefix converts a byte length to the signed 32-bit wire prefix. A length
// above math.MaxInt32 would wrap to a negative prefix that a reader takes for a
// corrupt buffer, so it is refused with ErrStringTooLong instead.
func lengthPrefix(n int) (int32, error) {
	if int64(n) > math.MaxInt32 {
		return 0, fmt.Errorf("%w: %d bytes, limit %d", ErrStringTooLong, n, math.MaxInt32)
	}
	return int32(n), nil
}

// WriteString writes a length-prefixed UTF-8 string (4-byte big-endian length + bytes).
// It returns ErrStringTooLong, and writes nothing, when s is over math.MaxInt32 bytes.
// It does not check that s is valid UTF-8; ReadString and the Rust engine do.
func (w *RustBufferWriter) WriteString(s string) error {
	n, err := lengthPrefix(len(s))
	if err != nil {
		return err
	}
	w.WriteInt32(n)
	w.buf = append(w.buf, s...)
	return nil
}

// WriteBytes writes a length-prefixed byte array (4-byte big-endian length + bytes).
// It returns ErrStringTooLong, and writes nothing, when b is over math.MaxInt32 bytes.
func (w *RustBufferWriter) WriteBytes(b []byte) error {
	n, err := lengthPrefix(len(b))
	if err != nil {
		return err
	}
	w.WriteInt32(n)
	w.buf = append(w.buf, b...)
	return nil
}

// RustBufferReader decodes big-endian typed primitives from a serialized byte stream.
type RustBufferReader struct {
	buf    []byte
	offset int
}

// NewReader constructs a new reader over data.
func NewReader(data []byte) *RustBufferReader {
	return &RustBufferReader{
		buf:    data,
		offset: 0,
	}
}

// Remaining returns the number of unconsumed bytes.
func (r *RustBufferReader) Remaining() int {
	return len(r.buf) - r.offset
}

// ReadInt8 reads an int8.
func (r *RustBufferReader) ReadInt8() (int8, error) {
	if r.Remaining() < 1 {
		return 0, ErrBufferUnderflow
	}
	v := int8(r.buf[r.offset])
	r.offset++
	return v, nil
}

// ReadUint8 reads a uint8.
func (r *RustBufferReader) ReadUint8() (uint8, error) {
	if r.Remaining() < 1 {
		return 0, ErrBufferUnderflow
	}
	v := r.buf[r.offset]
	r.offset++
	return v, nil
}

// ReadInt16 reads an int16 in big-endian order.
func (r *RustBufferReader) ReadInt16() (int16, error) {
	if r.Remaining() < 2 {
		return 0, ErrBufferUnderflow
	}
	v := int16(binary.BigEndian.Uint16(r.buf[r.offset : r.offset+2]))
	r.offset += 2
	return v, nil
}

// ReadInt32 reads an int32 in big-endian order.
func (r *RustBufferReader) ReadInt32() (int32, error) {
	if r.Remaining() < 4 {
		return 0, ErrBufferUnderflow
	}
	v := int32(binary.BigEndian.Uint32(r.buf[r.offset : r.offset+4]))
	r.offset += 4
	return v, nil
}

// ReadUint32 reads a uint32 in big-endian order.
func (r *RustBufferReader) ReadUint32() (uint32, error) {
	if r.Remaining() < 4 {
		return 0, ErrBufferUnderflow
	}
	v := binary.BigEndian.Uint32(r.buf[r.offset : r.offset+4])
	r.offset += 4
	return v, nil
}

// ReadInt64 reads an int64 in big-endian order.
func (r *RustBufferReader) ReadInt64() (int64, error) {
	if r.Remaining() < 8 {
		return 0, ErrBufferUnderflow
	}
	v := int64(binary.BigEndian.Uint64(r.buf[r.offset : r.offset+8]))
	r.offset += 8
	return v, nil
}

// ReadFloat64 reads a 64-bit IEEE float in big-endian order.
func (r *RustBufferReader) ReadFloat64() (float64, error) {
	if r.Remaining() < 8 {
		return 0, ErrBufferUnderflow
	}
	bits := binary.BigEndian.Uint64(r.buf[r.offset : r.offset+8])
	r.offset += 8
	return math.Float64frombits(bits), nil
}

// ReadBool reads a boolean.
func (r *RustBufferReader) ReadBool() (bool, error) {
	if r.Remaining() < 1 {
		return false, ErrBufferUnderflow
	}
	v := r.buf[r.offset] != 0
	r.offset++
	return v, nil
}

// ReadString reads a 4-byte length-prefixed UTF-8 string.
func (r *RustBufferReader) ReadString() (string, error) {
	l, err := r.ReadInt32()
	if err != nil {
		return "", err
	}
	if l < 0 {
		return "", ErrBufferUnderflow
	}
	if int(l) > r.Remaining() {
		return "", fmt.Errorf("%w: requested string of %d bytes, only %d available", ErrBufferUnderflow, l, r.Remaining())
	}
	raw := r.buf[r.offset : r.offset+int(l)]
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("%w: %d-byte string", ErrInvalidEncoding, l)
	}
	r.offset += int(l)
	return string(raw), nil
}

// ReadBytes reads a 4-byte length-prefixed byte slice.
func (r *RustBufferReader) ReadBytes() ([]byte, error) {
	l, err := r.ReadInt32()
	if err != nil {
		return nil, err
	}
	if l < 0 {
		return nil, ErrBufferUnderflow
	}
	if int(l) > r.Remaining() {
		return nil, fmt.Errorf("%w: requested %d bytes, only %d available", ErrBufferUnderflow, l, r.Remaining())
	}
	out := make([]byte, l)
	copy(out, r.buf[r.offset:r.offset+int(l)])
	r.offset += int(l)
	return out, nil
}
