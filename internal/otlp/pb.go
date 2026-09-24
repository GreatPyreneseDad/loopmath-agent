// Minimal protobuf wire-format reader for OTLP traces. We decode only the
// fields loopmath needs and skip everything else, so this stays ~200 lines
// and stdlib-only. Field numbers from opentelemetry-proto v1:
//
//	ExportTraceServiceRequest { repeated ResourceSpans resource_spans = 1; }
//	ResourceSpans   { Resource resource = 1; repeated ScopeSpans scope_spans = 2; }
//	Resource        { repeated KeyValue attributes = 1; }
//	ScopeSpans      { InstrumentationScope scope = 1; repeated Span spans = 2; }
//	Span            { bytes trace_id = 1; bytes span_id = 2; string name = 5;
//	                  fixed64 start_time_unix_nano = 7; fixed64 end_time_unix_nano = 8;
//	                  repeated KeyValue attributes = 9; repeated Event events = 11;
//	                  Status status = 15; }
//	Event           { fixed64 time_unix_nano = 1; string name = 2; repeated KeyValue attributes = 3; }
//	Status          { string message = 2; StatusCode code = 3; }
//	KeyValue        { string key = 1; AnyValue value = 2; }
//	AnyValue        { oneof: string_value=1 bool_value=2 int_value=3 double_value=4
//	                  ArrayValue array_value=5 KeyValueList kvlist_value=6 bytes bytes_value=7 }
//	ArrayValue      { repeated AnyValue values = 1; }
//	KeyValueList    { repeated KeyValue values = 1; }
package otlp

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var errTrunc = errors.New("protobuf: truncated")

type reader struct {
	b []byte
	i int
}

func (r *reader) more() bool { return r.i < len(r.b) }

func (r *reader) varint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.i:])
	if n <= 0 {
		return 0, errTrunc
	}
	r.i += n
	return v, nil
}

// tag returns field number and wire type.
func (r *reader) tag() (int, int, error) {
	v, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	return int(v >> 3), int(v & 7), nil
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	if uint64(len(r.b)-r.i) < n {
		return nil, errTrunc
	}
	out := r.b[r.i : r.i+int(n)]
	r.i += int(n)
	return out, nil
}

func (r *reader) fixed64() (uint64, error) {
	if len(r.b)-r.i < 8 {
		return 0, errTrunc
	}
	v := binary.LittleEndian.Uint64(r.b[r.i:])
	r.i += 8
	return v, nil
}

func (r *reader) skip(wt int) error {
	switch wt {
	case 0:
		_, err := r.varint()
		return err
	case 1:
		_, err := r.fixed64()
		return err
	case 2:
		_, err := r.bytes()
		return err
	case 5:
		if len(r.b)-r.i < 4 {
			return errTrunc
		}
		r.i += 4
		return nil
	}
	return fmt.Errorf("protobuf: unsupported wire type %d", wt)
}

// ---- decoded shapes (shared with the JSON path) ----

type Span struct {
	TraceID   string
	SpanID    string
	Name      string
	StartNano uint64
	EndNano   uint64
	Attrs     map[string]string // scalar attributes stringified; nested values JSON-ish
	Events    []Event
	Status    int // 0 unset, 1 ok, 2 error
	Resource  map[string]string
	Scope     string
}

type Event struct {
	Name  string
	Attrs map[string]string
}

// anyValue decodes an AnyValue into a string. Arrays/maps are rendered as a
// compact pseudo-JSON so message content in gen_ai.* attributes still yields
// shingles.
func anyValue(b []byte) (string, error) {
	r := &reader{b: b}
	for r.more() {
		f, wt, err := r.tag()
		if err != nil {
			return "", err
		}
		switch f {
		case 1: // string
			s, err := r.bytes()
			return string(s), err
		case 2: // bool
			v, err := r.varint()
			return strconv.FormatBool(v != 0), err
		case 3: // int
			v, err := r.varint()
			return strconv.FormatInt(int64(v), 10), err
		case 4: // double
			v, err := r.fixed64()
			return strconv.FormatFloat(math.Float64frombits(v), 'g', -1, 64), err
		case 5: // array
			av, err := r.bytes()
			if err != nil {
				return "", err
			}
			ar := &reader{b: av}
			var parts []string
			for ar.more() {
				_, wt2, err := ar.tag()
				if err != nil {
					return "", err
				}
				if wt2 != 2 {
					ar.skip(wt2)
					continue
				}
				el, err := ar.bytes()
				if err != nil {
					return "", err
				}
				s, err := anyValue(el)
				if err != nil {
					return "", err
				}
				parts = append(parts, s)
			}
			return "[" + strings.Join(parts, ",") + "]", nil
		case 6: // kvlist
			kv, err := r.bytes()
			if err != nil {
				return "", err
			}
			m, err := keyValues(kv, 1)
			if err != nil {
				return "", err
			}
			var parts []string
			for k, v := range m {
				parts = append(parts, k+":"+v)
			}
			return "{" + strings.Join(parts, ",") + "}", nil
		case 7: // bytes
			bs, err := r.bytes()
			return hex.EncodeToString(bs), err
		default:
			if err := r.skip(wt); err != nil {
				return "", err
			}
		}
	}
	return "", nil
}

// keyValues decodes a message consisting of `repeated KeyValue` at field fnum.
func keyValues(b []byte, fnum int) (map[string]string, error) {
	out := map[string]string{}
	r := &reader{b: b}
	for r.more() {
		f, wt, err := r.tag()
		if err != nil {
			return nil, err
		}
		if f != fnum || wt != 2 {
			if err := r.skip(wt); err != nil {
				return nil, err
			}
			continue
		}
		kvb, err := r.bytes()
		if err != nil {
			return nil, err
		}
		k, v, err := keyValue(kvb)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func keyValue(b []byte) (string, string, error) {
	r := &reader{b: b}
	var k, v string
	for r.more() {
		f, wt, err := r.tag()
		if err != nil {
			return "", "", err
		}
		switch {
		case f == 1 && wt == 2:
			kb, err := r.bytes()
			if err != nil {
				return "", "", err
			}
			k = string(kb)
		case f == 2 && wt == 2:
			vb, err := r.bytes()
			if err != nil {
				return "", "", err
			}
			v, err = anyValue(vb)
			if err != nil {
				return "", "", err
			}
		default:
			if err := r.skip(wt); err != nil {
				return "", "", err
			}
		}
	}
	return k, v, nil
}

func decodeEvent(b []byte) (Event, error) {
	r := &reader{b: b}
	ev := Event{Attrs: map[string]string{}}
	for r.more() {
		f, wt, err := r.tag()
		if err != nil {
			return ev, err
		}
		switch {
		case f == 2 && wt == 2:
			nb, err := r.bytes()
			if err != nil {
				return ev, err
			}
			ev.Name = string(nb)
		case f == 3 && wt == 2:
			kvb, err := r.bytes()
			if err != nil {
				return ev, err
			}
			k, v, err := keyValue(kvb)
			if err != nil {
				return ev, err
			}
			ev.Attrs[k] = v
		default:
			if err := r.skip(wt); err != nil {
				return ev, err
			}
		}
	}
	return ev, nil
}

func decodeSpan(b []byte, res map[string]string, scope string) (Span, error) {
	r := &reader{b: b}
	s := Span{Attrs: map[string]string{}, Resource: res, Scope: scope}
	for r.more() {
		f, wt, err := r.tag()
		if err != nil {
			return s, err
		}
		switch {
		case f == 1 && wt == 2:
			v, err := r.bytes()
			if err != nil {
				return s, err
			}
			s.TraceID = hex.EncodeToString(v)
		case f == 2 && wt == 2:
			v, err := r.bytes()
			if err != nil {
				return s, err
			}
			s.SpanID = hex.EncodeToString(v)
		case f == 5 && wt == 2:
			v, err := r.bytes()
			if err != nil {
				return s, err
			}
			s.Name = string(v)
		case f == 7 && wt == 1:
			s.StartNano, err = r.fixed64()
			if err != nil {
				return s, err
			}
		case f == 8 && wt == 1:
			s.EndNano, err = r.fixed64()
			if err != nil {
				return s, err
			}
		case f == 9 && wt == 2:
			kvb, err := r.bytes()
			if err != nil {
				return s, err
			}
			k, v, err := keyValue(kvb)
			if err != nil {
				return s, err
			}
			s.Attrs[k] = v
		case f == 11 && wt == 2:
			eb, err := r.bytes()
			if err != nil {
				return s, err
			}
			ev, err := decodeEvent(eb)
			if err != nil {
				return s, err
			}
			s.Events = append(s.Events, ev)
		case f == 15 && wt == 2:
			sb, err := r.bytes()
			if err != nil {
				return s, err
			}
			sr := &reader{b: sb}
			for sr.more() {
				sf, swt, err := sr.tag()
				if err != nil {
					return s, err
				}
				if sf == 3 && swt == 0 {
					c, err := sr.varint()
					if err != nil {
						return s, err
					}
					s.Status = int(c)
				} else if err := sr.skip(swt); err != nil {
					return s, err
				}
			}
		default:
			if err := r.skip(wt); err != nil {
				return s, err
			}
		}
	}
	return s, nil
}

// DecodeProto decodes an ExportTraceServiceRequest.
func DecodeProto(b []byte) ([]Span, error) {
	var out []Span
	r := &reader{b: b}
	for r.more() {
		f, wt, err := r.tag()
		if err != nil {
			return nil, err
		}
		if f != 1 || wt != 2 {
			if err := r.skip(wt); err != nil {
				return nil, err
			}
			continue
		}
		rsb, err := r.bytes()
		if err != nil {
			return nil, err
		}
		// ResourceSpans
		rr := &reader{b: rsb}
		res := map[string]string{}
		var scopeBlobs [][]byte
		for rr.more() {
			rf, rwt, err := rr.tag()
			if err != nil {
				return nil, err
			}
			switch {
			case rf == 1 && rwt == 2:
				resb, err := rr.bytes()
				if err != nil {
					return nil, err
				}
				res, err = keyValues(resb, 1)
				if err != nil {
					return nil, err
				}
			case rf == 2 && rwt == 2:
				ssb, err := rr.bytes()
				if err != nil {
					return nil, err
				}
				scopeBlobs = append(scopeBlobs, ssb)
			default:
				if err := rr.skip(rwt); err != nil {
					return nil, err
				}
			}
		}
		for _, ssb := range scopeBlobs {
			sr := &reader{b: ssb}
			scope := ""
			for sr.more() {
				sf, swt, err := sr.tag()
				if err != nil {
					return nil, err
				}
				switch {
				case sf == 1 && swt == 2: // InstrumentationScope { string name = 1 }
					scb, err := sr.bytes()
					if err != nil {
						return nil, err
					}
					scr := &reader{b: scb}
					for scr.more() {
						nf, nwt, err := scr.tag()
						if err != nil {
							return nil, err
						}
						if nf == 1 && nwt == 2 {
							nb, err := scr.bytes()
							if err != nil {
								return nil, err
							}
							scope = string(nb)
						} else if err := scr.skip(nwt); err != nil {
							return nil, err
						}
					}
				case sf == 2 && swt == 2:
					spb, err := sr.bytes()
					if err != nil {
						return nil, err
					}
					sp, err := decodeSpan(spb, res, scope)
					if err != nil {
						return nil, err
					}
					out = append(out, sp)
				default:
					if err := sr.skip(swt); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return out, nil
}
