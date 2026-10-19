package room

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
)

// Only dimensions are decoded, as with PNG/JPEG DecodeConfig. Pixel decoding
// and resizing belong to the image consumer. WebP RIFF/VP8X/VP8L layout:
// https://developers.google.com/speed/webp/docs/riff_container
func attachmentConfig(b []byte) (image.Config, string, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return image.DecodeConfig(bytes.NewReader(b))
	}
	invalid := fmt.Errorf("잘못된 WEBP 헤더")
	if len(b) < 20 || uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) {
		return image.Config{}, "", invalid
	}
	var width, height int
	hasPixels := false
	for pos := 12; pos < len(b); {
		if len(b)-pos < 8 {
			return image.Config{}, "", invalid
		}
		kind := string(b[pos : pos+4])
		size := uint64(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		end := uint64(pos) + 8 + size
		next := end + size%2
		if next > uint64(len(b)) {
			return image.Config{}, "", invalid
		}
		p := b[pos+8 : int(end)]
		switch kind {
		case "VP8X":
			if pos != 12 || len(p) != 10 {
				return image.Config{}, "", invalid
			}
			width = 1 + int(p[4]) + int(p[5])<<8 + int(p[6])<<16
			height = 1 + int(p[7]) + int(p[8])<<8 + int(p[9])<<16
		case "VP8 ":
			if len(p) < 10 || p[0]&1 != 0 || !bytes.Equal(p[3:6], []byte{0x9d, 0x01, 0x2a}) {
				return image.Config{}, "", invalid
			}
			if width == 0 {
				width = int(binary.LittleEndian.Uint16(p[6:8]) & 0x3fff)
				height = int(binary.LittleEndian.Uint16(p[8:10]) & 0x3fff)
			}
			hasPixels = true
		case "VP8L":
			if len(p) < 5 || p[0] != 0x2f || p[4]>>5 != 0 {
				return image.Config{}, "", invalid
			}
			bits := binary.LittleEndian.Uint32(p[1:5])
			if width == 0 {
				width = int(bits&0x3fff) + 1
				height = int((bits>>14)&0x3fff) + 1
			}
			hasPixels = true
		case "ANMF":
			if len(p) < 24 || width == 0 {
				return image.Config{}, "", invalid
			}
			hasPixels = true
		}
		pos = int(next)
	}
	if width <= 0 || height <= 0 || !hasPixels {
		return image.Config{}, "", invalid
	}
	return image.Config{Width: width, Height: height}, "webp", nil
}
