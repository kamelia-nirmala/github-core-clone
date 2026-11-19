// Git protocol primitives: pkt-line framing, packfile read/write, delta.
// Ported from git-pack.ts. zlib streams are inflated with a counting
// ByteReader so the exact consumed length is known (like Node's info:true).
package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const PktLineMax = 65520

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func IsObjectID(v string) bool { return oidRe.MatchString(v) }

// ------------------------------------------------------------- pkt-line

// Encode one pkt-line: 4 hex length digits + payload.
func EncodePktLine(payload []byte) []byte {
	length := len(payload) + 4
	if length > PktLineMax {
		panic(fmt.Sprintf("pkt-line payload %d exceeds the maximum", len(payload)))
	}
	head := fmt.Sprintf("%04x", length)
	out := make([]byte, 0, length)
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

func EncodePktLineString(s string) []byte { return EncodePktLine([]byte(s)) }

func PktLineFlush() []byte       { return []byte("0000") }
func PktLineDelim() []byte       { return []byte("0001") }
func PktLineResponseEnd() []byte { return []byte("0002") }

// Encode a list of lines / flush / delim markers into pkt-lines.
// Lines are strings ending with \n; markers are "flush" | "delim" | "response-end".
func EncodePktLines(lines []string) []byte {
	var out []byte
	for _, line := range lines {
		switch line {
		case "flush":
			out = append(out, PktLineFlush()...)
		case "delim":
			out = append(out, PktLineDelim()...)
		case "response-end":
			out = append(out, PktLineResponseEnd()...)
		default:
			out = append(out, EncodePktLineString(line)...)
		}
	}
	return out
}

type GitPacket struct {
	Kind    string // data | flush | delim | response-end
	Payload []byte
}

// Decode a whole buffer into packets (no trailing data allowed unless allowTrailing).
func DecodePktLines(data []byte, allowTrailing bool) ([]GitPacket, error) {
	packets := []GitPacket{}
	off := 0
	for off < len(data) {
		if off+4 > len(data) {
			if allowTrailing {
				break
			}
			return nil, errors.New("truncated pkt-line length")
		}
		head := string(data[off : off+4])
		if !isHex4(head) {
			if allowTrailing {
				break
			}
			return nil, fmt.Errorf("bad pkt-line length %q", head)
		}
		length := hex4(head)
		if length == 0 {
			packets = append(packets, GitPacket{Kind: "flush"})
			off += 4
			continue
		}
		if length == 1 {
			packets = append(packets, GitPacket{Kind: "delim"})
			off += 4
			continue
		}
		if length == 2 {
			packets = append(packets, GitPacket{Kind: "response-end"})
			off += 4
			continue
		}
		if length < 4 || off+length > len(data) {
			if allowTrailing {
				break
			}
			return nil, errors.New("pkt-line length out of range")
		}
		packets = append(packets, GitPacket{Kind: "data", Payload: data[off+4 : off+length]})
		off += length
	}
	return packets, nil
}

func isHex4(s string) bool {
	for i := 0; i < 4; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func hex4(s string) int {
	n := 0
	for i := 0; i < 4; i++ {
		c := s[i]
		n *= 16
		switch {
		case c >= '0' && c <= '9':
			n += int(c - '0')
		case c >= 'a' && c <= 'f':
			n += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int(c-'A') + 10
		}
	}
	return n
}

// The data lines of a decoded packet list, as strings (trailing \n stripped).
func PacketLines(packets []GitPacket) []string {
	out := []string{}
	for _, p := range packets {
		if p.Kind == "data" {
			out = append(out, stringsTrimSuffixNewline(string(p.Payload)))
		}
	}
	return out
}

func stringsTrimSuffixNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

// ------------------------------------------------------------- sideband

// Wrap raw bytes into sideband band-1 pkt-lines.
func SidebandPack(data []byte) []byte {
	var out []byte
	const chunk = 65520 - 4 - 1
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		framed := make([]byte, 0, 1+end-off)
		framed = append(framed, 1)
		framed = append(framed, data[off:end]...)
		out = append(out, EncodePktLine(framed)...)
	}
	if len(data) == 0 {
		out = append(out, EncodePktLine([]byte{1})...)
	}
	return out
}

// ------------------------------------------------------------- packfile

type PackObject struct {
	Type string
	Data []byte
	OID  string
}

// countingReader is a ByteReader over a slice that records how many bytes
// were consumed. compress/flate reads exactly what it needs from a
// ByteReader, so the consumed count is the true zlib stream length.
type countingReader struct {
	b   []byte
	off int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

func (r *countingReader) ReadByte() (byte, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	c := r.b[r.off]
	r.off++
	return c, nil
}

// Inflate one zlib stream at `offset`; returns its bytes and the offset just
// past the stream.
func InflateAt(buf []byte, offset int) ([]byte, int, error) {
	cr := &countingReader{b: buf[offset:]}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	if cr.off <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return data, offset + cr.off, nil
}

// Apply a git delta (copy/insert instruction stream) to a base object's bytes.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

type PackReadResult struct {
	Objects    map[string]*PackObject
	DeltaCount int
}

func readVarintSize(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errors.New("pack entry size varint is too long")
		}
	}
	return size, offset, first, nil
}

func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta has no base offset")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[offset]
		offset++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, offset, nil
}

// Read a packfile (version 2/3), resolving OFS_DELTA and REF_DELTA entries.
// resolveBase supplies a base object a thin pack leaves out.
func ReadPack(pack []byte, resolveBase func(oid string) *GitObject) (*PackReadResult, error) {
	buf := pack
	if len(buf) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(buf))
	}
	if string(buf[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := uint32(buf[4])<<24 | uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(uint32(buf[8])<<24 | uint32(buf[9])<<16 | uint32(buf[10])<<8 | uint32(buf[11]))
	trailerStart := len(buf) - 20
	trailer := hex.EncodeToString(buf[trailerStart:])
	h := sha1.Sum(buf[:trailerStart])
	if trailer != hex.EncodeToString(h[:]) {
		return nil, errors.New("pack trailer does not match its bytes")
	}

	type rawEntry struct {
		offset     int
		typeCode    int
		size       int
		data       []byte
		baseOffset int
		baseOid    string
		resolved   []byte
		resolvedTp string
		oid        string
	}
	raw := make([]*rawEntry, 0, declaredCount)
	byOffset := map[int]*rawEntry{}
	offset := 12
	for i := 0; i < declaredCount; i++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(buf, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		typeCode := int((first >> 4) & 0x07)
		entry := &rawEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			baseOff, next2, err := readOfsDeltaBase(buf, offset)
			if err != nil {
				return nil, err
			}
			offset = next2
			if baseOff <= 0 || baseOff > entryOffset {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOffset, baseOff)
			}
			entry.baseOffset = entryOffset - baseOff
		} else if typeCode == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOid = hex.EncodeToString(buf[offset : offset+20])
			offset += 20
		} else if typeCode < 1 || typeCode > 4 {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
		}
		inflated, nextOff, err := InflateAt(buf[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	objects := map[string]*PackObject{}
	resolving := map[int]bool{}
	resolvedByOid := map[string]*rawEntry{}
	var resolve func(entry *rawEntry) (string, []byte, string, error)
	resolve = func(entry *rawEntry) (string, []byte, string, error) {
		if entry.resolved != nil && entry.resolvedTp != "" && entry.oid != "" {
			return entry.resolvedTp, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var typ string
		var data []byte
		if entry.typeCode == 6 {
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				return "", nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
			}
			bt, bd, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = bt
			data, err = ApplyDelta(bd, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOid
			if baseEntry := resolvedByOid[wanted]; baseEntry != nil {
				bt, bd, _, err := resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = bt
				data, err = ApplyDelta(bd, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				var external *GitObject
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", wanted)
				}
				typ = external.Type
				var err error
				data, err = ApplyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			typ = codeType(entry.typeCode)
			data = entry.data
		}
		oid := HashObject(typ, data)
		entry.resolved = data
		entry.resolvedTp = typ
		entry.oid = oid
		resolvedByOid[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}

	deltaCount := 0
	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &PackObject{Type: typ, Data: data, OID: oid}
		}
	}
	return &PackReadResult{Objects: objects, DeltaCount: deltaCount}, nil
}

func codeType(code int) string {
	switch code {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

func typeCode(typ string) int {
	switch typ {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 0
}

func writePackObjectHeader(typ string, size int) []byte {
	code := typeCode(typ)
	var out []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining = remaining / 16
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining = remaining / 128
	}
	out = append(out, first)
	return out
}

// Build a packfile from plain objects (no deltas). Trailer is SHA-1 over everything before it.
func WritePack(objects []PackObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	buf.Write(header)
	for _, obj := range objects {
		buf.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		zw := zlib.NewWriter(&buf)
		zw.Write(obj.Data)
		zw.Close()
	}
	body := buf.Bytes()
	h := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, h[:]...)
	return out
}

// Deflate one object (used by tests/benchmarks).
func deflate(data []byte) []byte {
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, flate.DefaultCompression)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}
