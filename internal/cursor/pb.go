package cursor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"math"
	"sort"
)

type pb struct{ buf []byte }

func (p *pb) varint(field int, v uint64) *pb {
	p.buf = appendVarint(p.buf, uint64(field)<<3)
	p.buf = appendVarint(p.buf, v)
	return p
}

func (p *pb) bytes(field int, b []byte) *pb {
	p.buf = appendVarint(p.buf, uint64(field)<<3|2)
	p.buf = appendVarint(p.buf, uint64(len(b)))
	p.buf = append(p.buf, b...)
	return p
}

func (p *pb) str(field int, s string) *pb { return p.bytes(field, []byte(s)) }

func (p *pb) embed(field int, inner *pb) *pb {
	if inner == nil {
		return p.bytes(field, nil)
	}
	return p.bytes(field, inner.buf)
}

func (p *pb) double(field int, v float64) *pb {
	var bits [8]byte
	binary.LittleEndian.PutUint64(bits[:], math.Float64bits(v))
	p.buf = appendVarint(p.buf, uint64(field)<<3|1)
	p.buf = append(p.buf, bits[:]...)
	return p
}

func (p *pb) done() []byte { return append([]byte(nil), p.buf...) }

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

type field struct {
	num  int
	wire int
	n    uint64
	data []byte
}

func fields(b []byte) []field {
	var out []field
	i := 0
	uv := func() (uint64, bool) {
		var v uint64
		var s uint
		for i < len(b) && s < 64 {
			c := b[i]
			i++
			v |= uint64(c&0x7f) << s
			if c < 0x80 {
				return v, true
			}
			s += 7
		}
		return 0, false
	}
	for i < len(b) {
		key, ok := uv()
		if !ok {
			break
		}
		f := field{num: int(key >> 3), wire: int(key & 7)}
		switch f.wire {
		case 0:
			n, ok := uv()
			if !ok {
				return out
			}
			f.n = n
		case 1:
			if i+8 > len(b) {
				return out
			}
			f.data = b[i : i+8]
			f.n = binary.LittleEndian.Uint64(f.data)
			i += 8
		case 2:
			n, ok := uv()
			if !ok || i+int(n) > len(b) {
				return out
			}
			f.data = b[i : i+int(n)]
			i += int(n)
		case 5:
			if i+4 > len(b) {
				return out
			}
			f.n = uint64(binary.LittleEndian.Uint32(b[i : i+4]))
			i += 4
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}

func fieldBy(fs []field, num int) *field {
	for i := range fs {
		if fs[i].num == num {
			return &fs[i]
		}
	}
	return nil
}

func pbStr(fs []field, num int) string {
	f := fieldBy(fs, num)
	if f == nil || f.wire != 2 {
		return ""
	}
	return string(f.data)
}

func pbNum(fs []field, num int) uint64 {
	f := fieldBy(fs, num)
	if f == nil {
		return 0
	}
	return f.n
}

func pbValue(v any) *pb {
	switch x := v.(type) {
	case nil:
		return (&pb{}).varint(1, 0)
	case float64:
		return (&pb{}).double(2, x)
	case string:
		return (&pb{}).str(3, x)
	case bool:
		n := uint64(0)
		if x {
			n = 1
		}
		return (&pb{}).varint(4, n)
	case []any:
		l := &pb{}
		for _, e := range x {
			l.embed(1, pbValue(e))
		}
		return (&pb{}).embed(6, l)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		st := &pb{}
		for _, k := range keys {
			st.embed(1, (&pb{}).str(1, k).embed(2, pbValue(x[k])))
		}
		return (&pb{}).embed(5, st)
	default:
		return (&pb{}).varint(1, 0)
	}
}

func pbAny(b []byte) any {
	for _, f := range fields(b) {
		switch f.num {
		case 1:
			return nil
		case 2:
			if len(f.data) == 8 {
				return math.Float64frombits(binary.LittleEndian.Uint64(f.data))
			}
			return float64(0)
		case 3:
			return string(f.data)
		case 4:
			return f.n != 0
		case 5:
			m := map[string]any{}
			for _, e := range fields(f.data) {
				if e.num != 1 {
					continue
				}
				kv := fields(e.data)
				v := fieldBy(kv, 2)
				if v == nil {
					m[pbStr(kv, 1)] = nil
					continue
				}
				m[pbStr(kv, 1)] = pbAny(v.data)
			}
			return m
		case 6:
			var list []any
			for _, e := range fields(f.data) {
				if e.num == 1 {
					list = append(list, pbAny(e.data))
				}
			}
			return list
		}
	}
	return nil
}

func frame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

func readFrames(r io.Reader, fn func(end bool, data []byte) error) error {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	for {
		n, errRead := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		for len(buf) >= 5 {
			size := int(binary.BigEndian.Uint32(buf[1:5]))
			if size > 64<<20 {
				return io.ErrUnexpectedEOF
			}
			if len(buf) < 5+size {
				break
			}
			flags := buf[0]
			data := append([]byte(nil), buf[5:5+size]...)
			buf = append([]byte(nil), buf[5+size:]...)
			if flags&1 != 0 {
				zr, err := gzip.NewReader(bytes.NewReader(data))
				if err != nil {
					return err
				}
				plain, err := io.ReadAll(zr)
				_ = zr.Close()
				if err != nil {
					return err
				}
				data = plain
			}
			if err := fn(flags&2 != 0, data); err != nil {
				return err
			}
		}
		if errRead == io.EOF {
			if len(buf) != 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if errRead != nil {
			return errRead
		}
	}
}
