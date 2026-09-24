package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// OTLP/JSON per the protobuf JSON mapping. Trace/span ids are base64 in the
// spec but hex in practice from several exporters; accept both.

type jAnyValue struct {
	String *string                       `json:"stringValue"`
	Bool   *bool                         `json:"boolValue"`
	Int    *json.Number                  `json:"intValue"`
	Double *float64                      `json:"doubleValue"`
	Array  *struct{ Values []jAnyValue } `json:"arrayValue"`
	KV     *struct{ Values []jKeyValue } `json:"kvlistValue"`
	Bytes  *string                       `json:"bytesValue"`
}

type jKeyValue struct {
	Key   string    `json:"key"`
	Value jAnyValue `json:"value"`
}

func (v jAnyValue) str() string {
	switch {
	case v.String != nil:
		return *v.String
	case v.Bool != nil:
		return strconv.FormatBool(*v.Bool)
	case v.Int != nil:
		return v.Int.String()
	case v.Double != nil:
		return strconv.FormatFloat(*v.Double, 'g', -1, 64)
	case v.Array != nil:
		parts := make([]string, 0, len(v.Array.Values))
		for _, e := range v.Array.Values {
			parts = append(parts, e.str())
		}
		return "[" + strings.Join(parts, ",") + "]"
	case v.KV != nil:
		parts := make([]string, 0, len(v.KV.Values))
		for _, e := range v.KV.Values {
			parts = append(parts, e.Key+":"+e.Value.str())
		}
		return "{" + strings.Join(parts, ",") + "}"
	case v.Bytes != nil:
		return *v.Bytes
	}
	return ""
}

func kvMap(kvs []jKeyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value.str()
	}
	return m
}

func idHex(s string) string {
	if s == "" {
		return ""
	}
	if b, err := hex.DecodeString(s); err == nil && (len(b) == 16 || len(b) == 8) {
		return s
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return hex.EncodeToString(b)
	}
	return s
}

type jReq struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []jKeyValue `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Scope struct {
				Name string `json:"name"`
			} `json:"scope"`
			Spans []struct {
				TraceID    string      `json:"traceId"`
				SpanID     string      `json:"spanId"`
				Name       string      `json:"name"`
				Start      json.Number `json:"startTimeUnixNano"`
				End        json.Number `json:"endTimeUnixNano"`
				Attributes []jKeyValue `json:"attributes"`
				Events     []struct {
					Name       string      `json:"name"`
					Attributes []jKeyValue `json:"attributes"`
				} `json:"events"`
				Status struct {
					Code json.RawMessage `json:"code"`
				} `json:"status"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

func statusCode(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "STATUS_CODE_OK":
			return 1
		case "STATUS_CODE_ERROR":
			return 2
		}
	}
	return 0
}

func u64(n json.Number) uint64 {
	v, _ := strconv.ParseUint(n.String(), 10, 64)
	return v
}

// DecodeJSON decodes an OTLP/JSON ExportTraceServiceRequest.
func DecodeJSON(b []byte) ([]Span, error) {
	var req jReq
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	var out []Span
	for _, rs := range req.ResourceSpans {
		res := kvMap(rs.Resource.Attributes)
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				s := Span{
					TraceID: idHex(sp.TraceID), SpanID: idHex(sp.SpanID), Name: sp.Name,
					StartNano: u64(sp.Start), EndNano: u64(sp.End),
					Attrs: kvMap(sp.Attributes), Status: statusCode(sp.Status.Code),
					Resource: res, Scope: ss.Scope.Name,
				}
				for _, ev := range sp.Events {
					s.Events = append(s.Events, Event{Name: ev.Name, Attrs: kvMap(ev.Attributes)})
				}
				out = append(out, s)
			}
		}
	}
	return out, nil
}
