package extproc

import (
	"math"
	"net/http"
	"strings"
)

// Envoy ext_proc v3 messages (envoy/service/ext_proc/v3/external_processor.proto),
// with the field numbers of that file.

// Which part of the HTTP exchange a ProcessingRequest carries (its oneof).
const (
	reqHeaders   = 2
	respHeaders  = 3
	reqBody      = 4
	respBody     = 5
	reqTrailers  = 6
	respTrailers = 7
)

// BodySendMode values (ProcessingMode.BodySendMode).
const (
	modeNone            = 0
	modeStreamed        = 1
	modeBuffered        = 2
	modeBufferedPartial = 3
	modeFullDuplex      = 4
)

// processingRequest is the part of a ProcessingRequest the picker uses.
type processingRequest struct {
	kind        int
	headers     http.Header // request or response headers, pseudo-headers included (":path")
	endOfStream bool
	body        []byte
	// protocol is set when Envoy sent its ProtocolConfiguration (on the
	// first message of a stream, Envoy 1.33+).
	protocol *protocolConfig
	// metadata is metadata_context.filter_metadata: namespace -> fields.
	metadata map[string]map[string]any
}

type protocolConfig struct{ requestBody, responseBody int }

func decodeProcessingRequest(b []byte) (*processingRequest, error) {
	pr := &processingRequest{}
	err := fields(b, func(num, wire int, v uint64, sub []byte) error {
		switch {
		case wire != wireBytes:
		case num == reqHeaders || num == respHeaders:
			pr.kind = num
			return decodeHttpHeaders(sub, pr)
		case num == reqBody || num == respBody:
			pr.kind = num
			return fields(sub, func(n, w int, v uint64, b []byte) error {
				switch {
				case n == 1 && w == wireBytes:
					pr.body = b
				case n == 2 && w == wireVarint:
					pr.endOfStream = v != 0
				}
				return nil
			})
		case num == reqTrailers || num == respTrailers:
			pr.kind = num
		case num == 8: // metadata_context
			md, err := decodeMetadata(sub)
			pr.metadata = md
			return err
		case num == 11: // protocol_config
			pc := &protocolConfig{}
			pr.protocol = pc
			return fields(sub, func(n, w int, v uint64, _ []byte) error {
				if w == wireVarint {
					switch n {
					case 1:
						pc.requestBody = int(v)
					case 2:
						pc.responseBody = int(v)
					}
				}
				return nil
			})
		}
		return nil
	})
	return pr, err
}

// decodeHttpHeaders reads HttpHeaders{headers: HeaderMap = 1, end_of_stream = 3}.
func decodeHttpHeaders(b []byte, pr *processingRequest) error {
	pr.headers = http.Header{}
	return fields(b, func(num, wire int, v uint64, sub []byte) error {
		switch {
		case num == 3 && wire == wireVarint:
			pr.endOfStream = v != 0
		case num == 1 && wire == wireBytes: // HeaderMap{repeated HeaderValue headers = 1}
			return fields(sub, func(n, w int, _ uint64, hv []byte) error {
				if n != 1 || w != wireBytes {
					return nil
				}
				var key, value string
				err := fields(hv, func(n, w int, _ uint64, s []byte) error {
					if w == wireBytes {
						switch n {
						case 1:
							key = string(s)
						case 2, 3: // value, or raw_value (what Envoy sends by default now)
							value = string(s)
						}
					}
					return nil
				})
				if key != "" {
					// Keep pseudo-headers' colon; canonicalize the rest like net/http.
					if strings.HasPrefix(key, ":") {
						pr.headers[key] = append(pr.headers[key], value)
					} else {
						pr.headers.Add(key, value)
					}
				}
				return err
			})
		}
		return nil
	})
}

// decodeMetadata reads config.core.v3.Metadata{map<string, Struct> filter_metadata = 1}.
func decodeMetadata(b []byte) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	err := fields(b, func(num, wire int, _ uint64, entry []byte) error {
		if num != 1 || wire != wireBytes {
			return nil
		}
		var key string
		var val map[string]any
		err := fields(entry, func(n, w int, _ uint64, s []byte) error {
			if w != wireBytes {
				return nil
			}
			switch n {
			case 1:
				key = string(s)
			case 2:
				var err error
				val, err = decodeStruct(s)
				return err
			}
			return nil
		})
		if key != "" {
			out[key] = val
		}
		return err
	})
	return out, err
}

// decodeStruct reads google.protobuf.Struct{map<string, Value> fields = 1}.
func decodeStruct(b []byte) (map[string]any, error) {
	out := map[string]any{}
	err := fields(b, func(num, wire int, _ uint64, entry []byte) error {
		if num != 1 || wire != wireBytes {
			return nil
		}
		var key string
		var val any
		err := fields(entry, func(n, w int, _ uint64, s []byte) error {
			if w != wireBytes {
				return nil
			}
			switch n {
			case 1:
				key = string(s)
			case 2:
				var err error
				val, err = decodeValue(s)
				return err
			}
			return nil
		})
		out[key] = val
		return err
	})
	return out, err
}

// decodeValue reads google.protobuf.Value: null 1, number 2, string 3,
// bool 4, struct 5, list 6 (ListValue{repeated Value values = 1}).
func decodeValue(b []byte) (any, error) {
	var out any
	err := fields(b, func(num, wire int, v uint64, s []byte) error {
		var err error
		switch num {
		case 2:
			out = math.Float64frombits(v)
		case 3:
			out = string(s)
		case 4:
			out = v != 0
		case 5:
			out, err = decodeStruct(s)
		case 6:
			list := []any{}
			err = fields(s, func(n, w int, _ uint64, item []byte) error {
				if n == 1 && w == wireBytes {
					x, err := decodeValue(item)
					list = append(list, x)
					return err
				}
				return nil
			})
			out = list
		}
		return err
	})
	return out, err
}

// ProcessingResponse oneof fields.
const (
	outReqHeaders   = 1
	outRespHeaders  = 2
	outReqBody      = 3
	outRespBody     = 4
	outReqTrailers  = 5
	outRespTrailers = 6
	outImmediate    = 7
)

// overwrite is HeaderValueOption.append_action OVERWRITE_IF_EXISTS_OR_ADD:
// a client cannot pick its own endpoint by sending the header itself.
const overwrite = 2

func headerMutation(set [][2]string, remove []string) enc {
	var m enc
	for _, kv := range set {
		hv := enc(nil).str(1, kv[0]).bytes(3, []byte(kv[1])) // key, raw_value
		m = m.msg(1, enc(nil).msg(1, hv).varint(3, overwrite))
	}
	for _, k := range remove {
		m = m.str(2, k)
	}
	return m
}

// commonResponse builds CommonResponse{header_mutation = 2,
// body_mutation = 3, clear_route_cache = 5}.
func commonResponse(set [][2]string, remove []string, bodyMutation enc, clearRoute bool) enc {
	var c enc
	if len(set) > 0 || len(remove) > 0 {
		c = c.msg(2, headerMutation(set, remove))
	}
	if bodyMutation != nil {
		c = c.msg(3, bodyMutation)
	}
	return c.boolean(5, clearRoute)
}

// streamedBody is BodyMutation{streamed_response = 3: {body = 1, end_of_stream = 2}}.
func streamedBody(chunk []byte, end bool) enc {
	return enc(nil).msg(3, enc(nil).bytes(1, chunk).boolean(2, end))
}

// replaceBody is BodyMutation{body = 1}, for buffered mode.
func replaceBody(body []byte) enc { return enc(nil).bytes(1, body) }

// processingResponse wraps one oneof case, plus dynamic_metadata (8) when
// md is non-empty: namespace -> string fields.
func processingResponse(field int, inner enc, md map[string]map[string]string) []byte {
	out := enc(nil).msg(field, inner)
	if len(md) > 0 {
		out = out.msg(8, structOf(md))
	}
	return out
}

func structOf(md map[string]map[string]string) enc {
	var s enc
	for ns, kv := range md {
		var inner enc
		for k, v := range kv {
			inner = inner.msg(1, enc(nil).str(1, k).msg(2, enc(nil).str(3, v)))
		}
		s = s.msg(1, enc(nil).str(1, ns).msg(2, enc(nil).msg(5, inner)))
	}
	return s
}

// immediateResponse builds ImmediateResponse{status = 1: HttpStatus{code = 1},
// headers = 2, body = 3, details = 5}.
func immediateResponse(status int, headers [][2]string, body []byte, details string) enc {
	im := enc(nil).msg(1, enc(nil).varint(1, uint64(status)))
	if len(headers) > 0 {
		im = im.msg(2, headerMutation(headers, nil))
	}
	if len(body) > 0 {
		im = im.bytes(3, body)
	}
	if details != "" {
		im = im.str(5, details)
	}
	return im
}
