package muzak

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// The JSON-RPC 2.0 error codes the MCP endpoint answers with: the five the
// specification defines, and the two of MCP's own range a 2026-07-28 client
// recognises a modern server by.
const (
	rpcParseError         = -32700
	rpcInvalidRequest     = -32600
	rpcMethodNotFound     = -32601
	rpcInvalidParams      = -32602
	rpcInternalError      = -32603
	rpcHeaderMismatch     = -32020
	rpcUnsupportedVersion = -32022
)

// Bounds on a message beyond the body limit it is read under.
const (
	// maxRPCDepth bounds how deeply a message nests. A tool's arguments are
	// a route's input, which is rarely more than a few levels deep, and a
	// bound keeps a body of brackets from costing a recursive descent in
	// everything that reads it afterwards.
	maxRPCDepth = 128
	// maxRPCIDLength bounds the text of a request id, which is echoed in the
	// answer: a client's own counter or identifier, never a document.
	maxRPCIDLength = 256
)

// rpcKind is what a message is.
type rpcKind uint8

const (
	rpcRequest rpcKind = iota + 1
	rpcNotification
	rpcResponse
)

// rpcMessage is one JSON-RPC message, read strictly: its id is the raw JSON
// of a string or a number, echoed in the answer exactly as it was sent, and
// its params the raw JSON of an object, read by whichever method it is for.
type rpcMessage struct {
	kind   rpcKind
	id     jsontext.Value
	method string
	params jsontext.Value
}

// rpcError is a JSON-RPC error object. Its message is always one of the fixed
// sentences below: nothing a client sent and nothing a server failure says
// is quoted in one.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitzero"`
}

// The errors a malformed message is answered with.
var (
	errRPCParse        = &rpcError{Code: rpcParseError, Message: "Parse error: the message is not valid JSON"}
	errRPCNotObject    = &rpcError{Code: rpcInvalidRequest, Message: "Invalid Request: a message is one JSON object"}
	errRPCBatch        = &rpcError{Code: rpcInvalidRequest, Message: "Invalid Request: a batch is not accepted; send each message in a request of its own"}
	errRPCTooDeep      = &rpcError{Code: rpcInvalidRequest, Message: "Invalid Request: the message is nested too deeply"}
	errRPCShape        = &rpcError{Code: rpcInvalidRequest, Message: "Invalid Request: the message is not a JSON-RPC 2.0 request, notification or response"}
	errRPCParamsObject = &rpcError{Code: rpcInvalidParams, Message: "Invalid params: params must be an object"}
)

// rpcEnvelope is every member a JSON-RPC message may carry, each kept raw so
// that its type can be checked rather than coerced.
type rpcEnvelope struct {
	JSONRPC jsontext.Value `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Method  jsontext.Value `json:"method"`
	Params  jsontext.Value `json:"params"`
	Result  jsontext.Value `json:"result"`
	Error   jsontext.Value `json:"error"`
}

// parseRPCMessage reads one message. It returns the message, or an error to
// answer with; when the message got far enough to have an id, the message is
// returned beside the error so that the answer can carry it.
//
// The reading is strict and linear in the size of the message: the JSON must
// be valid UTF-8 with no member named twice at any depth, nested at most
// maxRPCDepth deep, and one object holding only the members JSON-RPC 2.0
// defines, with "jsonrpc" exactly "2.0". A request's id is a string or a
// number and never null, as MCP requires. An array is a batch, which the
// revisions from 2025-06-18 removed, and is refused.
func parseRPCMessage(data []byte) (*rpcMessage, *rpcError) {
	value := jsontext.Value(data)
	if !value.IsValid() {
		return nil, errRPCParse
	}
	if jsonDepthExceeds(data, maxRPCDepth) {
		return nil, errRPCTooDeep
	}
	switch value.Kind() {
	case '{':
	case '[':
		return nil, errRPCBatch
	default:
		return nil, errRPCNotObject
	}
	var env rpcEnvelope
	if err := json.Unmarshal(data, &env, json.RejectUnknownMembers(true)); err != nil {
		return nil, errRPCShape
	}
	if version, ok := rawString(env.JSONRPC); !ok || version != "2.0" {
		return nil, errRPCShape
	}
	msg := &rpcMessage{}
	if env.ID != nil {
		switch kind := env.ID.Kind(); {
		case kind != '"' && kind != '0', len(env.ID) > maxRPCIDLength:
			return nil, errRPCShape
		}
		msg.id = env.ID
	}
	hasResult, hasError := env.Result != nil, env.Error != nil
	if env.Method != nil {
		method, ok := rawString(env.Method)
		if !ok || hasResult || hasError {
			return nil, errRPCShape
		}
		msg.method = method
		msg.kind = rpcNotification
		if msg.id != nil {
			msg.kind = rpcRequest
		}
		if env.Params != nil {
			if env.Params.Kind() != '{' {
				return msg, errRPCParamsObject
			}
			msg.params = env.Params
		}
		return msg, nil
	}
	if msg.id == nil || hasResult == hasError || env.Params != nil || (hasError && env.Error.Kind() != '{') {
		return nil, errRPCShape
	}
	msg.kind = rpcResponse
	return msg, nil
}

// rawString decodes raw JSON that must be a string.
func rawString(raw jsontext.Value) (string, bool) {
	if raw == nil || raw.Kind() != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// coverage: raw is a valid JSON string, which always decodes into one.
		return "", false
	}
	return s, true
}

// jsonDepthExceeds reports whether valid JSON nests deeper than limit. It is a
// single pass that skips the contents of strings, so a bracket quoted in one
// is not counted.
func jsonDepthExceeds(data []byte, limit int) bool {
	depth, inString, escaped := 0, false, false
	for _, b := range data {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
		case b == '"':
			inString = true
		case b == '{' || b == '[':
			if depth++; depth > limit {
				return true
			}
		case b == '}' || b == ']':
			depth--
		}
	}
	return false
}

// rpcNullID is the id of an answer to a message whose own could not be read.
var rpcNullID = jsontext.Value("null")

// rpcAnswer is a JSON-RPC response the endpoint sends.
type rpcAnswer struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id,omitzero"`
	Result  any            `json:"result,omitzero"`
	Error   *rpcError      `json:"error,omitzero"`
}

// encodeRPCResult writes the answer to a request that succeeded.
func encodeRPCResult(id jsontext.Value, result any) ([]byte, error) {
	return json.Marshal(rpcAnswer{JSONRPC: "2.0", ID: id, Result: result}, json.Deterministic(true))
}

// encodeRPCError writes an error answer. An id that could not be read is
// null, as JSON-RPC 2.0 has it, unless omit is set: the 2026-07-28 schema
// leaves the member out instead.
func encodeRPCError(id jsontext.Value, e *rpcError, omit bool) []byte {
	if id == nil && !omit {
		id = rpcNullID
	}
	data, err := json.Marshal(rpcAnswer{JSONRPC: "2.0", ID: id, Error: e}, json.Deterministic(true))
	if err != nil {
		// coverage: every error's data is built from strings and string
		// slices, which always encode.
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
	}
	return data
}
