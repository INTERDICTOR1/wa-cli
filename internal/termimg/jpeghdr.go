package termimg

import (
	"bufio"
	"encoding/binary"
	"io"
)

// JPEG markers the header scan cares about.
const (
	jpegSOF0  = 0xC0 // start of frame: baseline
	jpegSOF1  = 0xC1 // extended sequential
	jpegSOF2  = 0xC2 // progressive
	jpegSOI   = 0xD8 // start of image
	jpegEOI   = 0xD9 // end of image
	jpegSOS   = 0xDA // start of scan: the pixels begin, the headers are over
	jpegAPP0  = 0xE0 // JFIF
	jpegAPP1  = 0xE1 // EXIF (or XMP)
	jpegAPP14 = 0xEE // Adobe
)

// jpegHeader is what the segments before a JPEG's pixel data say: which way
// up the picture is, and enough about its layout to know what the standard
// library's decoder will allocate for it.
type jpegHeader struct {
	orientation int // EXIF orientation 1..8; 1 when there is none

	width, height int
	progressive   bool
	ncomp         int     // colour components: 1, 3 or 4; 0 if no frame header was found
	id            [4]byte // component identifiers
	h, v          [4]int  // sampling factors per component

	jfif           bool
	adobe          bool // has an Adobe segment, which says how to read the components
	adobeTransform byte
}

// scanJPEG walks the segments of a JPEG up to the start of the pixel data.
// It never fails: whatever can't be read keeps its default.
func scanJPEG(r *bufio.Reader) jpegHeader {
	j := jpegHeader{orientation: 1}
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 0xFF || b[1] != jpegSOI {
		return j
	}
	var seg []byte // reused for every segment worth reading
	for {
		// A segment starts with 0xFF, any number of 0xFF fill bytes, and
		// the marker. Like every decoder, skip stray bytes before it.
		c, err := r.ReadByte()
		if err != nil {
			return j
		}
		if c != 0xFF {
			continue
		}
		marker := c
		for marker == 0xFF {
			if marker, err = r.ReadByte(); err != nil {
				return j
			}
		}
		switch {
		case marker == jpegSOS || marker == jpegEOI:
			return j
		case marker == 0 || marker == 1 || marker >= 0xD0 && marker <= 0xD7:
			continue // markers that stand alone, without a length
		}
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return j
		}
		n := int(binary.BigEndian.Uint16(b[:])) - 2 // the length counts itself
		if n < 0 {
			return j
		}
		switch marker {
		case jpegSOF0, jpegSOF1, jpegSOF2, jpegAPP0, jpegAPP1, jpegAPP14:
			if cap(seg) < n {
				seg = make([]byte, n)
			}
			seg = seg[:n]
			if _, err := io.ReadFull(r, seg); err != nil {
				return j
			}
		default:
			if _, err := r.Discard(n); err != nil {
				return j
			}
			continue
		}
		switch marker {
		case jpegSOF0, jpegSOF1, jpegSOF2:
			j.frame(seg, marker == jpegSOF2)
		case jpegAPP0:
			j.jfif = j.jfif || len(seg) >= 5 && string(seg[:5]) == "JFIF\x00"
		case jpegAPP1:
			if j.orientation == 1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
				j.orientation = exifOrientation(seg[6:])
			}
		case jpegAPP14:
			if len(seg) >= 12 && string(seg[:5]) == "Adobe" {
				j.adobe, j.adobeTransform = true, seg[11]
			}
		}
	}
}

// frame reads a start-of-frame segment: precision, height, width, then per
// component an id, the sampling factors packed in one byte, and a table
// number.
func (j *jpegHeader) frame(seg []byte, progressive bool) {
	if len(seg) < 6 || j.ncomp != 0 {
		return
	}
	n := int(seg[5])
	if (n != 1 && n != 3 && n != 4) || len(seg) < 6+3*n {
		return
	}
	j.height = int(binary.BigEndian.Uint16(seg[1:]))
	j.width = int(binary.BigEndian.Uint16(seg[3:]))
	j.progressive = progressive
	for i := 0; i < n; i++ {
		j.id[i] = seg[6+3*i]
		j.h[i], j.v[i] = int(seg[7+3*i]>>4), int(seg[7+3*i]&0x0F)
	}
	j.ncomp = n
}

// exifOrientation finds the Orientation tag (0x0112) in an EXIF block, which
// is a small TIFF file: a byte-order mark ("II" little-endian, "MM"
// big-endian), the number 42, and the offset of a directory of 12-byte
// entries (tag, type, count, value). It returns 1 when there is none.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(tiff[2:]) != 42 {
		return 1
	}
	dir := int64(bo.Uint32(tiff[4:]))
	if dir+2 > int64(len(tiff)) {
		return 1
	}
	entries := int64(bo.Uint16(tiff[dir:]))
	for i := int64(0); i < entries; i++ {
		e := dir + 2 + 12*i
		if e+12 > int64(len(tiff)) {
			break
		}
		if bo.Uint16(tiff[e:]) != 0x0112 {
			continue
		}
		// The value sits left-aligned in the entry's last four bytes. It
		// should be a 16-bit SHORT; accept what careless writers use too.
		var v uint32
		switch bo.Uint16(tiff[e+2:]) {
		case 1: // BYTE
			v = uint32(tiff[e+8])
		case 3: // SHORT
			v = uint32(bo.Uint16(tiff[e+8:]))
		case 4: // LONG
			v = bo.Uint32(tiff[e+8:])
		}
		if v >= 1 && v <= 8 {
			return int(v)
		}
		return 1
	}
	return 1
}

// decodedBytes is what image/jpeg allocates to decode this file, worked out
// the way its decoder does (measured to match within 1%): 0 if unknown.
//
// The picture is cut into 8x8 blocks per component. A baseline file is
// decoded block by block straight into the output planes, one byte per
// sample. A progressive file spreads every block over several scans, so the
// decoder keeps all blocks as 64 32-bit coefficients (256 bytes) until the
// end: four more bytes per sample.
func (j *jpegHeader) decodedBytes() int64 {
	if j.ncomp == 0 {
		return 0
	}
	h, v := j.h, j.v
	if j.ncomp == 1 {
		h[0], v[0] = 1, 1 // a single component is never subsampled
	}
	maxH, maxV := 1, 1
	for i := 0; i < j.ncomp; i++ {
		if h[i] < 1 || h[i] > 4 || v[i] < 1 || v[i] > 4 {
			return 0 // the decoder will refuse it
		}
		maxH, maxV = max(maxH, h[i]), max(maxV, v[i])
	}
	// The picture is covered by whole MCUs: groups of blocks that hold
	// 8*maxH x 8*maxV pixels.
	mcus := int64((j.width+8*maxH-1)/(8*maxH)) * int64((j.height+8*maxV-1)/(8*maxV))
	var blocks int64
	for i := 0; i < j.ncomp; i++ {
		blocks += int64(h[i]*v[i]) * mcus
	}

	var total int64
	if j.ncomp == 1 {
		total = 64 * blocks
	} else {
		luma := 64 * int64(maxH*maxV) * mcus
		// Unusual subsamplings are stored as three full-size planes.
		total = 3 * luma
		if h[1] == h[2] && v[1] == v[2] && h[0] == maxH && v[0] == maxV {
			switch (maxH/h[1])<<4 | maxV/v[1] {
			case 0x11, 0x12, 0x21, 0x22, 0x41, 0x42:
				total = luma + 2*64*int64(h[1]*v[1])*mcus
			}
		}
		// CMYK and RGB files are decoded as above and then converted
		// into a second, 4 bytes/pixel image.
		pixels := int64(j.width) * int64(j.height)
		if j.ncomp == 4 {
			total += 64*int64(h[3]*v[3])*mcus + 4*pixels
		} else if j.isRGB() {
			total += 4 * pixels
		}
	}
	if j.progressive {
		total += 256 * blocks
	}
	return total
}

// isRGB reports whether a three-component file stores RGB rather than YCbCr,
// by the same rules as image/jpeg.
func (j *jpegHeader) isRGB() bool {
	if j.jfif {
		return false
	}
	if j.adobe && j.adobeTransform == 0 {
		return true
	}
	return j.id[0] == 'R' && j.id[1] == 'G' && j.id[2] == 'B'
}
