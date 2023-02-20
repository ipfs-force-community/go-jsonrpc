package jsonrpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
	"go.opencensus.io/trace/propagation"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-jsonrpc/metrics"
)

type RawParams json.RawMessage

var rtRawParams = reflect.TypeOf(RawParams{})

// todo is there a better way to tell 'struct with any number of fields'?
func DecodeParams[T any](p RawParams) (T, error) {
	var t T
	err := json.Unmarshal(p, &t)

	// todo also handle list-encoding automagically (json.Unmarshal doesn't do that, does it?)

	return t, err
}

// methodHandler is a handler for a single method
type methodHandler struct {
	paramReceivers []reflect.Type
	nParams        int

	receiver    reflect.Value
	handlerFunc reflect.Value

	hasCtx       int
	hasRawParams bool

	errOut int
	valOut int
}

// Request / response

type request struct {
	Jsonrpc string            `json:"jsonrpc"`
	ID      interface{}       `json:"id,omitempty"`
	Method  string            `json:"method"`
	Params  json.RawMessage   `json:"params"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// Limit request size. Ideally this limit should be specific for each field
// in the JSON request but as a simple defensive measure we just limit the
// entire HTTP body.
// Configured by WithMaxRequestSize.
const DEFAULT_MAX_REQUEST_SIZE = 100 << 20 // 100 MiB

type ErrorCode int

func (e ErrorCode) Error() string {
	return fmt.Sprintf("error code %d", e)
}

const (
	DefaultError ErrorCode = 0
	LogicError   ErrorCode = 1
	NetError     ErrorCode = 2
	AuthError    ErrorCode = 401

	//private error
	startRpcError     ErrorCode = -32768
	endRpcError       ErrorCode = -32000
	rpcParseError     ErrorCode = -32700
	rpcMethodNotFound ErrorCode = -32601
	rpcInvalidParams  ErrorCode = -32602
	rpcMarshalERROR   ErrorCode = -32603
	rpcWrongId        ErrorCode = -32604
	rpcExiting        ErrorCode = -32605
)

type respError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e respError) Error() string {
	if e.Code >= startRpcError && e.Code <= endRpcError {
		return fmt.Sprintf("RPC error (%d): %s", e.Code, e.Message)
	}
	return e.Message
}

func (e respError) Is(code error) bool {
	return errors.Is(e.Code, code)
}

type response struct {
	Jsonrpc string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	ID      interface{} `json:"id"`
	Error   error       `json:"error,omitempty"`
}

type handler struct {
	methods map[string]methodHandler

	maxRequestSize int64

	// aliasedMethods contains a map of alias:original method names.
	// These are used as fallbacks if a method is not found by the given method name.
	aliasedMethods map[string]string

	paramDecoders map[reflect.Type]ParamDecoder

	proxyBind ProxyBind
}

func makeHandler(sc ServerConfig) *handler {
	return &handler{
		methods: make(map[string]methodHandler),

		aliasedMethods: map[string]string{},
		paramDecoders:  sc.paramDecoders,

		maxRequestSize: sc.maxRequestSize,

		proxyBind: sc.proxyBind,
	}
}

// Register

func (s *handler) register(namespace string, r interface{}) {
	switch s.proxyBind {
	case PBField:
		s.registerWithField(namespace, r)
	case PBMethod:
		s.registerWithMethod(namespace, r)
	default:
		panic("Unknown rpcHandler proxy")
	}

}

func (s *handler) registerWithMethod(namespace string, r interface{}) {
	val := reflect.ValueOf(r)
	// TODO: expect ptr

	for i := 0; i < val.NumMethod(); i++ {
		method := val.Type().Method(i)

		funcType := method.Func.Type()
		hasCtx := 0
		if funcType.NumIn() >= 2 && funcType.In(1) == contextType {
			hasCtx = 1
		}

		hasRawParams := false
		ins := funcType.NumIn() - 1 - hasCtx
		recvs := make([]reflect.Type, ins)
		for i := 0; i < ins; i++ {
			recvs[i] = method.Type.In(i + 1 + hasCtx)
			if hasRawParams && i > 0 {
				panic("raw params must be the last parameter")
			}
			if funcType.In(i+1+hasCtx) == rtRawParams {
				hasRawParams = true
			}
		}

		valOut, errOut, _ := processFuncOut(funcType)

		s.methods[namespace+"."+method.Name] = methodHandler{
			paramReceivers: recvs,
			nParams:        ins,

			handlerFunc: method.Func,
			receiver:    val,

			hasCtx:       hasCtx,
			hasRawParams: hasRawParams,

			errOut: errOut,
			valOut: valOut,
		}
	}
}

func (s *handler) registerWithField(namespace string, r interface{}) {
	val := reflect.ValueOf(r).Elem()
	// TODO: expect ptr

	for i := 0; i < val.NumField(); i++ {
		s.registerInnerStructField(namespace, val.Field(i))
	}
}

func (s *handler) registerInnerStructField(namespace string, val reflect.Value) {
	for i := 0; i < val.NumField(); i++ {
		field := val.Type().Field(i)
		funcType := field.Type
		if funcType.Kind() == reflect.Struct {
			s.registerInnerStructField(namespace, val.Field(i))
		} else {
			hasCtx := 0
			if funcType.NumIn() >= 1 && funcType.In(0) == contextType {
				hasCtx = 1
			}

			hasRawParams := false
			ins := funcType.NumIn() - hasCtx
			recvs := make([]reflect.Type, ins)
			for i := 0; i < ins; i++ {
				recvs[i] = field.Type.In(i + hasCtx)
				if hasRawParams && i > 1 {
					panic("raw params must be the last parameter")
				}
				if funcType.In(i+hasCtx) == rtRawParams {
					hasRawParams = true
				}
			}

			valOut, errOut, _ := processFuncOut(funcType)

			s.methods[namespace+"."+field.Name] = methodHandler{
				paramReceivers: recvs,
				nParams:        ins,

				handlerFunc: val.Field(i),
				receiver:    val,

				hasCtx:       hasCtx,
				hasRawParams: hasRawParams,

				errOut: errOut,
				valOut: valOut,
			}
		}
	}
}

// Handle

type rpcErrFunc func(w func(func(io.Writer)), req *request, err error)
type chanOut func(reflect.Value, interface{}) error

func (s *handler) handleReader(ctx context.Context, r io.Reader, w io.Writer, rpcError rpcErrFunc) {
	wf := func(cb func(io.Writer)) {
		cb(w)
	}

	var req request
	// We read the entire request upfront in a buffer to be able to tell if the
	// client sent more than maxRequestSize and report it back as an explicit error,
	// instead of just silently truncating it and reporting a more vague parsing
	// error.
	bufferedRequest := new(bytes.Buffer)
	// We use LimitReader to enforce maxRequestSize. Since it won't return an
	// EOF we can't actually know if the client sent more than the maximum or
	// not, so we read one byte more over the limit to explicitly query that.
	// FIXME: Maybe there's a cleaner way to do this.
	reqSize, err := bufferedRequest.ReadFrom(io.LimitReader(r, s.maxRequestSize+1))
	if err != nil {
		// ReadFrom will discard EOF so any error here is unexpected and should
		// be reported.
		rpcError(wf, &req, xerrors.Errorf("(%w) reading request: %s", rpcParseError, err))
		return
	}
	if reqSize > s.maxRequestSize {
		rpcError(wf, &req,
			// rpcParseError is the closest we have from the standard errors defined
			// in [jsonrpc spec](https://www.jsonrpc.org/specification#error_object)
			// to report the maximum limit.
			xerrors.Errorf("(%w) request bigger than maximum %d allowed", rpcParseError, s.maxRequestSize))
		return
	}

	if err := json.NewDecoder(bufferedRequest).Decode(&req); err != nil {
		rpcError(wf, &req, xerrors.Errorf("(%w) unmarshaling request: %s", rpcParseError, err))
		return
	}

	if req.ID, err = normalizeID(req.ID); err != nil {
		rpcError(wf, &req, fmt.Errorf("(%w) failed to parse ID: %v", rpcParseError, err))
		return
	}

	s.handle(ctx, req, wf, rpcError, func(bool) {}, nil)
}

func doCall(methodName string, f reflect.Value, params []reflect.Value) (out []reflect.Value, err error) {
	defer func() {
		if i := recover(); i != nil {
			err = xerrors.Errorf("panic in rpc method '%s': %s", methodName, i)
			log.Desugar().WithOptions(zap.AddStacktrace(zapcore.ErrorLevel)).Sugar().Error(err)
		}
	}()

	out = f.Call(params)
	return out, nil
}

func (s *handler) getSpan(ctx context.Context, req request) (spCtx context.Context, span *trace.Span) {
	defer func() {
		if span == nil {
			spCtx, span = trace.StartSpan(ctx, "api.handle")
		}
		span.AddAttributes(trace.StringAttribute("method", req.Method))
	}()

	if req.Meta == nil {
		return
	}

	if eSC, ok := req.Meta["SpanContext"]; ok {
		bSC := make([]byte, base64.StdEncoding.DecodedLen(len(eSC)))
		_, err := base64.StdEncoding.Decode(bSC, []byte(eSC))
		if err != nil {
			log.Errorf("SpanContext: decode", "error", err)
			return ctx, nil
		}
		sc, ok := propagation.FromBinary(bSC)
		if !ok {
			log.Errorf("SpanContext: could not create span", "data", bSC)
			return ctx, nil
		}
		spCtx, span = trace.StartSpanWithRemoteParent(ctx, "api.handle", sc)
	}
	return
}

func (s *handler) handle(ctx context.Context, req request, w func(func(io.Writer)), rpcError rpcErrFunc, done func(keepCtx bool), chOut chanOut) {
	// Not sure if we need to sanitize the incoming req.Method or not.
	ctx, span := s.getSpan(ctx, req)
	ctx, _ = tag.New(ctx, tag.Insert(metrics.RPCMethod, req.Method))
	defer span.End()

	handler, ok := s.methods[req.Method]
	if !ok {
		aliasTo, ok := s.aliasedMethods[req.Method]
		if ok {
			handler, ok = s.methods[aliasTo]
		}
		if !ok {
			rpcError(w, &req, fmt.Errorf("(%w) method '%s' not found", rpcMethodNotFound, req.Method))
			stats.Record(ctx, metrics.RPCInvalidMethod.M(1))
			done(false)
			return
		}
	}

	outCh := handler.valOut != -1 && handler.handlerFunc.Type().Out(handler.valOut).Kind() == reflect.Chan
	defer done(outCh)

	if chOut == nil && outCh {
		rpcError(w, &req, fmt.Errorf("(%w) method '%s' not supported in this mode (no out channel support)", rpcMethodNotFound, req.Method))
		stats.Record(ctx, metrics.RPCRequestError.M(1))
		return
	}
	ctxParamIndex := 0
	if s.proxyBind == PBMethod {
		ctxParamIndex += 1
	}
	callParams := make([]reflect.Value, ctxParamIndex+handler.hasCtx+handler.nParams)
	callParams[0] = handler.receiver
	if handler.hasCtx == 1 {
		callParams[ctxParamIndex] = reflect.ValueOf(ctx)
	}

	if handler.hasRawParams {
		// When hasRawParams is true, there is only one parameter and it is a
		// json.RawMessage.

		callParams[1+handler.hasCtx] = reflect.ValueOf(RawParams(req.Params))
	} else {
		// "normal" param list; no good way to do named params in Golang

		var ps []param
		if len(req.Params) > 0 {
			err := json.Unmarshal(req.Params, &ps)
			if err != nil {
				rpcError(w, &req, xerrors.Errorf("(%w) unmarshaling param array: %v", rpcParseError, err))
				stats.Record(ctx, metrics.RPCRequestError.M(1))
				return
			}
		}

		if len(ps) != handler.nParams {
			rpcError(w, &req, fmt.Errorf("(%w) wrong param count (method '%s'): %d != %d", rpcInvalidParams, req.Method, len(ps), handler.nParams))
			stats.Record(ctx, metrics.RPCRequestError.M(1))
			done(false)
			return
		}

		for i := 0; i < handler.nParams; i++ {
			var rp reflect.Value

			typ := handler.paramReceivers[i]
			dec, found := s.paramDecoders[typ]
			if !found {
				rp = reflect.New(typ)
				if err := json.NewDecoder(bytes.NewReader(ps[i].data)).Decode(rp.Interface()); err != nil {
					rpcError(w, &req, xerrors.Errorf("(%w) unmarshaling params for '%s' (param: %T): %v", rpcParseError, req.Method, rp.Interface(), err))
					stats.Record(ctx, metrics.RPCRequestError.M(1))
					return
				}
				rp = rp.Elem()
			} else {
				var err error
				rp, err = dec(ctx, ps[i].data)
				if err != nil {
					rpcError(w, &req, xerrors.Errorf("(%w) decoding params for '%s' (param: %d; custom decoder): %v", rpcParseError, req.Method, i, err))
					stats.Record(ctx, metrics.RPCRequestError.M(1))
					return
				}
			}

			callParams[i+ctxParamIndex+handler.hasCtx] = reflect.ValueOf(rp.Interface())
		}
	}

	// /////////////////

	callResult, err := doCall(req.Method, handler.handlerFunc, callParams)
	if err != nil {
		rpcError(w, &req, xerrors.Errorf("(%w) fatal error calling '%s': %v", DefaultError, req.Method, err))
		stats.Record(ctx, metrics.RPCRequestError.M(1))
		return
	}
	if req.ID == nil {
		return // notification
	}

	// /////////////////

	resp := response{
		Jsonrpc: "2.0",
		ID:      req.ID,
	}
	//respinse must give resp error format.

	if handler.errOut != -1 {
		err := callResult[handler.errOut].Interface()
		if err != nil {
			log.Warnf("error in RPC call to '%s': %+v", req.Method, err)
			stats.Record(ctx, metrics.RPCResponseError.M(1))

			var code = LogicError //default code to 1
			_ = xerrors.As(err.(error), &code)
			resp.Error = &respError{
				Code:    code,
				Message: err.(error).Error(),
			}
		}
	}

	var kind reflect.Kind
	var res interface{}
	var nonZero bool
	if handler.valOut != -1 {
		res = callResult[handler.valOut].Interface()
		kind = callResult[handler.valOut].Kind()
		nonZero = !callResult[handler.valOut].IsZero()
	}

	// check error as JSON-RPC spec prohibits error and value at the same time
	if resp.Error == nil {
		if res != nil && kind == reflect.Chan {
			// Channel responses are sent from channel control goroutine.
			// Sending responses here could cause deadlocks on writeLk, or allow
			// sending channel messages before this rpc call returns

			//noinspection GoNilness // already checked above
			err = chOut(callResult[handler.valOut], req.ID)
			if err == nil {
				return // channel goroutine handles responding
			}

			log.Warnf("failed to setup channel in RPC call to '%s': %+v", req.Method, err)
			stats.Record(ctx, metrics.RPCResponseError.M(1))
			var code = LogicError //default code to 1
			_ = xerrors.As(err, &code)
			resp.Error = &respError{
				Code:    code,
				Message: err.Error(),
			}
		} else {
			resp.Result = res
		}
	}
	if resp.Error != nil && nonZero {
		log.Errorw("error and res returned", "request", req, "r.err", resp.Error, "res", res)
	}

	w(func(w io.Writer) {
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Error(err)
			stats.Record(ctx, metrics.RPCResponseError.M(1))
			return
		}
	})
}
