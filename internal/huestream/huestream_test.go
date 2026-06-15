package huestream

import "testing"

// v1 frame: header + two 9-byte light records (lights 6 and 11), RGB.
func v1Frame() []byte {
	b := []byte("HueStream")
	b = append(b, 0x01, 0x00) // major 1, minor 0
	b = append(b, 0x07)       // sequence
	b = append(b, 0x00, 0x00) // reserved
	b = append(b, 0x00)       // colorspace RGB
	b = append(b, 0x00)       // reserved
	// light 6: type 0, id 0x0006, R=0xFFFF G=0x0000 B=0x8000
	b = append(b, 0x00, 0x00, 0x06, 0xFF, 0xFF, 0x00, 0x00, 0x80, 0x00)
	// light 11: type 0, id 0x000B, R=0x0000 G=0xFFFF B=0x0000
	b = append(b, 0x00, 0x00, 0x0B, 0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00)
	return b
}

func TestParse_v1_RGB(t *testing.T) {
	f, err := Parse(v1Frame())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Major != 1 || f.Sequence != 7 || f.ColorSpaceName() != "rgb" {
		t.Fatalf("header = %+v", f)
	}
	if len(f.Channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(f.Channels))
	}
	if f.Channels[0] != (Channel{ID: 6, A: 0xFFFF, B: 0x0000, C: 0x8000}) {
		t.Fatalf("ch0 = %+v", f.Channels[0])
	}
	if f.Channels[1] != (Channel{ID: 11, A: 0x0000, B: 0xFFFF, C: 0x0000}) {
		t.Fatalf("ch1 = %+v", f.Channels[1])
	}
}

func TestParse_v2_XY_withConfigID(t *testing.T) {
	id := "abcdefab-1234-1234-1234-0123456789ab" // 36 chars
	b := []byte("HueStream")
	b = append(b, 0x02, 0x00) // major 2
	b = append(b, 0x05)       // sequence
	b = append(b, 0x00, 0x00)
	b = append(b, 0x01) // colorspace XY
	b = append(b, 0x00)
	b = append(b, []byte(id)...)
	// channel 0: X=0x1111 Y=0x2222 bri=0x3333
	b = append(b, 0x00, 0x11, 0x11, 0x22, 0x22, 0x33, 0x33)

	f, err := Parse(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Major != 2 || f.ConfigID != id || f.ColorSpaceName() != "xy" {
		t.Fatalf("header = %+v", f)
	}
	if len(f.Channels) != 1 || f.Channels[0] != (Channel{ID: 0, A: 0x1111, B: 0x2222, C: 0x3333}) {
		t.Fatalf("channels = %+v", f.Channels)
	}
}

func TestParse_rejectsBadMagicAndShort(t *testing.T) {
	if _, err := Parse([]byte("notHueStream....")); err == nil {
		t.Fatal("expected bad-magic error")
	}
	if _, err := Parse([]byte("HueStr")); err == nil {
		t.Fatal("expected short-frame error")
	}
}
