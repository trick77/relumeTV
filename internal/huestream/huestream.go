// Package huestream parses the Philips Hue Entertainment streaming protocol
// ("HueStream"), the binary, big-endian payload a Hue client (e.g. an Ambilight
// TV) sends over the DTLS stream on UDP :2100. Two layouts exist: v1 carries
// per-light ids, v2 carries an entertainment-configuration id plus channel ids.
package huestream

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const magic = "HueStream"

// Color spaces a frame's three values are expressed in.
const (
	ColorSpaceRGB = 0x00 // values are R, G, B (16-bit each)
	ColorSpaceXY  = 0x01 // values are X, Y, brightness (16-bit each)
)

// Channel is one light's (v1) or channel's (v2) target color in a frame. A/B/C
// are R/G/B in RGB color space, or X/Y/brightness in XY color space.
type Channel struct {
	ID uint16 // v1: light id (2 bytes); v2: channel id (one byte, 0-255)
	A  uint16
	B  uint16
	C  uint16
}

// Frame is a decoded HueStream message.
type Frame struct {
	Major      uint8
	Minor      uint8
	Sequence   uint8
	ColorSpace uint8
	ConfigID   string // v2 only: the entertainment configuration id (36 ASCII)
	Channels   []Channel
}

// ColorSpaceName returns "rgb" or "xy" for logging.
func (f *Frame) ColorSpaceName() string {
	if f.ColorSpace == ColorSpaceXY {
		return "xy"
	}
	return "rgb"
}

// Parse decodes a HueStream datagram. The 16-byte header is:
// "HueStream"(9) major(1) minor(1) seq(1) reserved(2) colorspace(1) reserved(1).
// v1 then has 9-byte light records (type, id[2], A[2], B[2], C[2]); v2 has a
// 36-byte config id then 7-byte channel records (id, A[2], B[2], C[2]).
func Parse(b []byte) (*Frame, error) {
	if len(b) < 16 {
		return nil, fmt.Errorf("huestream: short frame (%d bytes)", len(b))
	}
	if string(b[0:9]) != magic {
		return nil, errors.New("huestream: bad magic")
	}
	f := &Frame{Major: b[9], Minor: b[10], Sequence: b[11], ColorSpace: b[14]}
	body := b[16:]

	switch f.Major {
	case 1:
		const sz = 9 // type(1) + id(2) + 3x uint16
		for len(body) >= sz {
			f.Channels = append(f.Channels, Channel{
				ID: binary.BigEndian.Uint16(body[1:3]),
				A:  binary.BigEndian.Uint16(body[3:5]),
				B:  binary.BigEndian.Uint16(body[5:7]),
				C:  binary.BigEndian.Uint16(body[7:9]),
			})
			body = body[sz:]
		}
	case 2:
		if len(body) < 36 {
			return nil, fmt.Errorf("huestream: v2 frame too short for config id (%d bytes)", len(body))
		}
		f.ConfigID = string(body[:36])
		body = body[36:]
		const sz = 7 // id(1) + 3x uint16
		for len(body) >= sz {
			f.Channels = append(f.Channels, Channel{
				ID: uint16(body[0]),
				A:  binary.BigEndian.Uint16(body[1:3]),
				B:  binary.BigEndian.Uint16(body[3:5]),
				C:  binary.BigEndian.Uint16(body[5:7]),
			})
			body = body[sz:]
		}
	default:
		return nil, fmt.Errorf("huestream: unsupported version %d.%d", f.Major, f.Minor)
	}
	return f, nil
}
