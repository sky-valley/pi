// Package frame is pi-env's wire framing (docs/protocol.md "Frames"), shared
// by the daemon and its client:
//
//	u32 length | u8 type | u32 id | u32 jsonLength | json | payload
//
// All integers are big-endian; length counts the bytes after itself. The
// package moves raw JSON bytes and leaves their meaning to each side.
package frame

import (
	"encoding/binary"
	"errors"
	"io"
)

// Frame types.
const (
	Request byte = 1 // client → daemon; json.op names the operation
	Result  byte = 2 // daemon → client; success of request id, which is finished
	Error   byte = 3 // daemon → client; failure of request id, which is finished
	Event   byte = 4 // daemon → client; progress of a running request
	Cancel  byte = 5 // client → daemon; cancel request id
	Ping    byte = 6 // both; liveness, ignored by the receiver
)

const (
	// MaxFrame is the largest frame either side accepts, counting what
	// follows the length field.
	MaxFrame = 16 * 1024 * 1024
	// MaxPayload is the largest payload a response carries, leaving room for
	// its JSON.
	MaxPayload = MaxFrame - 64*1024

	headerLength = 9 // type, id, jsonLength
)

// Frame is one frame. JSON is the raw JSON object; an empty JSON field on the
// wire reads as nil, which the protocol treats as {}.
type Frame struct {
	Kind    byte
	ID      uint32
	JSON    []byte
	Payload []byte
}

var (
	// ErrCorrupt matches what Read reports for a stream that is not frames: a
	// length or JSON length out of range. Other errors are the reader's.
	ErrCorrupt = errors.New("corrupt frame")

	// ErrTooLarge is what Write reports for a frame above MaxFrame, which it
	// does not write: the other side would refuse it and end the connection.
	ErrTooLarge = errors.New("frame too large")

	// The daemon prints these as the Rust daemon does ("pi-env: <text>").
	errLength     error = corruptError("frame length out of range")
	errJSONLength error = corruptError("JSON length out of range")
)

// corruptError is a framing failure: its own text, matching ErrCorrupt.
type corruptError string

func (e corruptError) Error() string      { return string(e) }
func (corruptError) Is(target error) bool { return target == ErrCorrupt }

// Read reads one frame. It returns io.EOF at a clean end of input, which is
// an end before or inside the length field; an end inside the rest of a frame
// is io.ErrUnexpectedEOF, and a length out of range is an error that ends the
// connection (framing cannot recover from it).
func Read(r io.Reader) (Frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length < headerLength || length > MaxFrame {
		return Frame{}, errLength
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	jsonLength := binary.BigEndian.Uint32(body[5:9])
	if uint64(headerLength)+uint64(jsonLength) > uint64(length) {
		return Frame{}, errJSONLength
	}
	f := Frame{Kind: body[0], ID: binary.BigEndian.Uint32(body[1:5])}
	end := headerLength + int(jsonLength)
	if jsonLength > 0 {
		f.JSON = body[headerLength:end]
	}
	f.Payload = body[end:]
	return f, nil
}

// Write writes f as one frame. It fails with ErrTooLarge, without writing,
// when the frame would exceed MaxFrame; the caller flushes buffered writers.
func Write(w io.Writer, f Frame) error {
	length := headerLength + len(f.JSON) + len(f.Payload)
	if length > MaxFrame {
		return ErrTooLarge
	}
	var header [4 + headerLength]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(length))
	header[4] = f.Kind
	binary.BigEndian.PutUint32(header[5:9], f.ID)
	binary.BigEndian.PutUint32(header[9:13], uint32(len(f.JSON)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(f.JSON); err != nil {
		return err
	}
	_, err := w.Write(f.Payload)
	return err
}
