package jsonrpc

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log/v2"
	"go.opencensus.io/trace"
	"go.opencensus.io/trace/propagation"
	"golang.org/x/xerrors"
)

const (
	methodMinRetryDelay = 100 * time.Millisecond
	methodMaxRetryDelay = 10 * time.Minute
)

var (
	errorType   = reflect.TypeOf(new(error)).Elem()
	contextType = reflect.TypeOf(new(context.Context)).Elem()

	log = logging.Logger("rpc")

	_defaultHTTPClient = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
)

// ErrClient is an error which occurred on the client side the library
type ErrClient struct {
	err error
}

func (e *ErrClient) Error() string {
	return fmt.Sprintf("RPC client error: %s", e.err)
}

// Unwrap unwraps the actual error
func (e *ErrClient) Unwrap(err error) error {
	return e.err
}

type clientResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	ID      interface{}     `json:"id"`
	Error   error           `json:"error,omitempty"`
}

type makeChanSink func() (context.Context, func([]byte, bool))

type clientRequest struct {
	req   request
	ready chan clientResponse

	// retCh provides a context and sink for handling incoming channel messages
	retCh makeChanSink
}

// ClientCloser is used to close Client from further use
type ClientCloser func()

// NewClient creates new jsonrpc 2.0 client
//
// handler must be pointer to a struct with function fields
// Returned value closes the client connection
// TODO: Example
func NewClient(ctx context.Context, addr string, namespace string, handler interface{}, requestHeader http.Header) (ClientCloser, error) {
	return NewMergeClient(ctx, addr, namespace, []interface{}{handler}, requestHeader)
}

type client struct {
	namespace     string
	paramEncoders map[reflect.Type]ParamEncoder
	backoff       backoff
	retry         bool
	doRequest     func(context.Context, clientRequest) clientResponse
	exiting       <-chan struct{}
	idCtr         int64
}

// NewMergeClient is like NewClient, but allows to specify multiple structs
// to be filled in the same namespace, using one connection
func NewMergeClient(ctx context.Context, addr string, namespace string, outs []interface{}, requestHeader http.Header, opts ...Option) (ClientCloser, error) {
	config := defaultConfig()
	for _, o := range opts {
		o(&config)
	}

	u, err := url.Parse(addr)
	if err != nil {
		return nil, xerrors.Errorf("parsing address: %w", err)
	}

	switch u.Scheme {
	case "ws", "wss":
		return websocketClient(ctx, addr, namespace, outs, requestHeader, config)
	case "http", "https":
		return httpClient(ctx, addr, namespace, outs, requestHeader, config)
	default:
		return nil, xerrors.Errorf("unknown url scheme '%s'", u.Scheme)
	}

}

func httpClient(ctx context.Context, addr string, namespace string, outs []interface{}, requestHeader http.Header, config Config) (ClientCloser, error) {
	c := client{
		namespace:     namespace,
		paramEncoders: config.paramEncoders,
		retry:         config.retry,
		backoff:       config.reconnectBackoff,
	}

	stop := make(chan struct{})
	c.exiting = stop

	if requestHeader == nil {
		requestHeader = http.Header{}
	}

	c.doRequest = func(ctx context.Context, cr clientRequest) clientResponse {
		b, err := json.Marshal(&cr.req)
		if err != nil {
			return clientResponse{ID: cr.req.ID, Error: fmt.Errorf("(%w) marshaling requset: %s", rpcMarshalERROR, err)}
		}

		hreq, err := http.NewRequest("POST", addr, bytes.NewReader(b))
		if err != nil {
			return clientResponse{ID: cr.req.ID, Error: fmt.Errorf("(%w) request error %s", rpcInvalidParams, err)}
		}

		hreq.Header = requestHeader.Clone()

		if ctx == nil {
			ctx = context.Background()
		}
		hreq = hreq.WithContext(ctx)

		hreq.Header.Set("Content-Type", "application/json")

		httpResp, err := _defaultHTTPClient.Do(hreq) //todo cancel by timeout or neterror
		if err != nil {
			if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) ||
				strings.Contains(err.Error(), context.Canceled.Error()) {
				return clientResponse{ID: cr.req.ID, Error: fmt.Errorf("(%w) cancel by context %s", rpcExiting, err)}
			} else {
				return clientResponse{ID: cr.req.ID, Error: fmt.Errorf("(%w) do request error %s", NetError, err)}
			}
		}
		defer httpResp.Body.Close()

		var respFrame frame
		var res clientResponse
		if cr.req.ID != nil { // non-notification
			if err := json.NewDecoder(httpResp.Body).Decode(&respFrame); err != nil {
				return clientResponse{ID: cr.req.ID, Error: xerrors.Errorf("(%w) http status %s unmarshaling response: %s", rpcParseError, httpResp.Status, err)}
			}
			if respFrame.ID, err = normalizeID(respFrame.ID); err != nil {
				return clientResponse{ID: cr.req.ID, Error: fmt.Errorf("(%w) failed to response ID: %s", rpcWrongId, err)}
			}
			if respFrame.ID != cr.req.ID {
				return clientResponse{ID: cr.req.ID, Error: xerrors.Errorf("(%w) request and response id didn't match", rpcWrongId)}
			}

			res = clientResponse{
				Jsonrpc: respFrame.Jsonrpc,
				ID:      respFrame.ID,
				Result:  respFrame.Result,
			}
			if respFrame.Error != nil {
				res.Error = respFrame.Error
			}
		}
		return res
	}

	if err := c.provide(outs); err != nil {
		return nil, err
	}

	return func() {
		close(stop)
	}, nil
}

func websocketClient(ctx context.Context, addr string, namespace string, outs []interface{}, requestHeader http.Header, config Config) (ClientCloser, error) {
	connFactory := func() (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.Dial(addr, requestHeader)
		if err != nil {
			return conn, xerrors.Errorf("cannot dialer to addr %s due to %v", addr, err)
		}
		return conn, nil
	}

	if config.proxyConnFactory != nil {
		// used in tests
		connFactory = config.proxyConnFactory(connFactory)
	}

	conn, err := connFactory()
	if err != nil {
		return nil, err
	}

	if config.noReconnect {
		connFactory = nil
	}

	c := client{
		namespace:     namespace,
		backoff:       config.reconnectBackoff,
		paramEncoders: config.paramEncoders,
		retry:         config.retry,
	}

	requests := c.setupRequestChan()

	stop := make(chan struct{})
	exiting := make(chan struct{})
	c.exiting = exiting

	var hnd reqestHandler
	if len(config.reverseHandlers) > 0 {
		h := makeHandler(defaultServerConfig())
		h.aliasedMethods = config.aliasedHandlerMethods
		for _, reverseHandler := range config.reverseHandlers {
			h.register(reverseHandler.ns, reverseHandler.hnd)
		}
		hnd = h
	}

	go (&wsConn{
		conn:             conn,
		connFactory:      connFactory,
		reconnectBackoff: config.reconnectBackoff,
		pingInterval:     config.pingInterval,
		timeout:          config.timeout,
		handler:          hnd,
		requests:         requests,
		stop:             stop,
		exiting:          exiting,
	}).handleWsConn(ctx)

	if err := c.provide(outs); err != nil {
		return nil, err
	}

	return func() {
		close(stop)
		<-exiting
	}, nil
}

func (c *client) setupRequestChan() chan clientRequest {
	requests := make(chan clientRequest)

	c.doRequest = func(ctx context.Context, cr clientRequest) clientResponse {
		select {
		case requests <- cr:
		case <-c.exiting:
			return clientResponse{ID: cr.req.ID, Error: fmt.Errorf("(%w) websocket routine exiting", rpcExiting)}
		}

		var ctxDone <-chan struct{}
		var resp clientResponse

		if ctx != nil {
			ctxDone = ctx.Done()
		}

		// wait for response, handle context cancellation
	loop:
		for {
			select {
			case resp = <-cr.ready:
				break loop
			case <-ctxDone: // send cancel request
				ctxDone = nil

				rp, err := json.Marshal([]param{{v: reflect.ValueOf(cr.req.ID)}})
				if err != nil {
					return clientResponse{ID: cr.req.ID, Error: xerrors.Errorf("(%w) marshalling cancel request: %v", rpcMarshalERROR, err)}
				}

				cancelReq := clientRequest{
					req: request{
						Jsonrpc: "2.0",
						Method:  wsCancel,
						Params:  rp,
					},
					ready: make(chan clientResponse, 1),
				}
				select {
				case requests <- cancelReq:
				case <-c.exiting:
					log.Warn("failed to send request cancellation, websocket routing exited")
				}

				return clientResponse{ID: cr.req.ID, Error: fmt.Errorf("(%w) context by cancel", rpcExiting)}
			}
		}

		return resp
	}

	return requests
}

func (c *client) provide(outs []interface{}) error {
	for _, handler := range outs {
		htyp := reflect.TypeOf(handler)
		if htyp.Kind() != reflect.Ptr {
			return xerrors.New("expected handler to be a pointer")
		}
		typ := htyp.Elem()
		if typ.Kind() != reflect.Struct {
			return xerrors.New("handler should be a struct")
		}

		val := reflect.ValueOf(handler).Elem()
		err := c.provideStruct(val)
		if err != nil {
			return err
		}
		/*for i := 0; i < typ.NumField(); i++ {
			fn, err := c.makeRpcFunc(typ.Field(i))
			if err != nil {
				return err
			}

			val.Elem().Field(i).Set(fn)
		}*/
	}
	return nil
}

func (c *client) provideStruct(val reflect.Value) error {
	for i := 0; i < val.NumField(); i++ {
		field := val.Type().Field(i)
		funcType := field.Type
		if funcType.Kind() == reflect.Struct {
			c.provideStruct(val.Field(i))
		} else {
			fn, err := c.makeRpcFunc(field)
			if err != nil {
				return err
			}
			val.Field(i).Set(fn)
		}
	}
	return nil
}

func (c *client) makeOutChan(ctx context.Context, ftyp reflect.Type, valOut int) (func() reflect.Value, makeChanSink) {
	retVal := reflect.Zero(ftyp.Out(valOut))

	chCtor := func() (context.Context, func([]byte, bool)) {
		// unpack chan type to make sure it's reflect.BothDir
		ctyp := reflect.ChanOf(reflect.BothDir, ftyp.Out(valOut).Elem())
		ch := reflect.MakeChan(ctyp, 0) // todo: buffer?
		retVal = ch.Convert(ftyp.Out(valOut))

		incoming := make(chan reflect.Value, 32)

		// gorotuine to handle buffering of items
		go func() {
			buf := (&list.List{}).Init()

			for {
				front := buf.Front()

				cases := []reflect.SelectCase{
					{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(ctx.Done()),
					},
					{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(incoming),
					},
				}

				if front != nil {
					cases = append(cases, reflect.SelectCase{
						Dir:  reflect.SelectSend,
						Chan: ch,
						Send: front.Value.(reflect.Value).Elem(),
					})
				}

				chosen, val, ok := reflect.Select(cases)

				switch chosen {
				case 0:
					ch.Close()
					return
				case 1:
					if ok {
						vvval := val.Interface().(reflect.Value)
						buf.PushBack(vvval)
						if buf.Len() > 1 {
							if buf.Len() > 10 {
								log.Warnw("rpc output message buffer", "n", buf.Len())
							} else {
								log.Debugw("rpc output message buffer", "n", buf.Len())
							}
						}
					} else {
						incoming = nil
					}

				case 2:
					buf.Remove(front)
				}

				if incoming == nil && buf.Len() == 0 {
					ch.Close()
					return
				}
			}
		}()

		return ctx, func(result []byte, ok bool) {
			if !ok {
				close(incoming)
				return
			}

			val := reflect.New(ftyp.Out(valOut).Elem())
			if err := json.Unmarshal(result, val.Interface()); err != nil {
				log.Errorf("error unmarshaling chan response: %s", err)
				return
			}

			if ctx.Err() != nil {
				log.Errorf("got rpc message with cancelled context: %s", ctx.Err())
				return
			}

			select {
			case incoming <- val:
			case <-ctx.Done():
			}
		}
	}

	return func() reflect.Value { return retVal }, chCtor
}

func (c *client) sendRequest(ctx context.Context, req request, chCtor makeChanSink) clientResponse {
	creq := clientRequest{
		req:   req,
		ready: make(chan clientResponse, 1),

		retCh: chCtor,
	}

	return c.doRequest(ctx, creq)
}

type rpcFunc struct {
	client *client

	ftyp reflect.Type
	name string

	nout   int
	valOut int
	errOut int

	// hasCtx is 1 if the function has a context.Context as its first argument.
	// Used as the number of the first non-context argument.
	hasCtx               int
	returnValueIsChannel bool
	hasRawParams         bool

	retry   bool
	backoff backoff
	notify  bool
}

func (fn *rpcFunc) processResponse(resp clientResponse, rval reflect.Value) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		out[fn.valOut] = rval
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		if resp.Error != nil {
			out[fn.errOut].Set(reflect.ValueOf(resp.Error))
		}
	}

	return out
}

func (fn *rpcFunc) processError(err error) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		out[fn.valOut] = reflect.New(fn.ftyp.Out(fn.valOut)).Elem()
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		out[fn.errOut].Set(reflect.ValueOf(&ErrClient{err}))
	}

	return out
}

func (fn *rpcFunc) handleRpcCall(args []reflect.Value) (results []reflect.Value) {
	var id interface{}
	if !fn.notify {
		id = atomic.AddInt64(&fn.client.idCtr, 1)

		// Prepare the ID to send on the wire.
		// We track int64 ids as float64 in the inflight map (because that's what
		// they'll be decoded to). encoding/json outputs numbers with their minimal
		// encoding, avoding the decimal point when possible, i.e. 3 will never get
		// converted to 3.0.
		var err error
		id, err = normalizeID(id)
		if err != nil {
			return fn.processError(fmt.Errorf("failed to normalize id")) // should probably panic
		}
	}

	var serializedParams json.RawMessage

	if fn.hasRawParams {
		serializedParams = json.RawMessage(args[fn.hasCtx].Interface().(RawParams))
	} else {
		params := make([]param, len(args)-fn.hasCtx)
		for i, arg := range args[fn.hasCtx:] {
			enc, found := fn.client.paramEncoders[arg.Type()]
			if found {
				// custom param encoder
				var err error
				arg, err = enc(arg)
				if err != nil {
					return fn.processError(fmt.Errorf("sendRequest failed: %w", err))
				}
			}

			params[i] = param{
				v: arg,
			}
		}
		var err error
		serializedParams, err = json.Marshal(params)
		if err != nil {
			return fn.processError(fmt.Errorf("marshaling params failed: %w", err))
		}
	}

	var ctx context.Context
	var span *trace.Span
	if fn.hasCtx == 1 {
		ctx = args[0].Interface().(context.Context)
		ctx, span = trace.StartSpan(ctx, "api.call")
		defer span.End()
	}

	retVal := func() reflect.Value { return reflect.Value{} }

	// if the function returns a channel, we need to provide a sink for the
	// messages
	var chCtor makeChanSink
	if fn.returnValueIsChannel {
		retVal, chCtor = fn.client.makeOutChan(ctx, fn.ftyp, fn.valOut)
	}

	req := request{
		Jsonrpc: "2.0",
		ID:      id,
		Method:  fn.name,
		Params:  serializedParams,
	}

	if span != nil {
		span.AddAttributes(trace.StringAttribute("method", req.Method))

		eSC := base64.StdEncoding.EncodeToString(
			propagation.Binary(span.SpanContext()))
		req.Meta = map[string]string{
			"SpanContext": eSC,
		}
	}

	var resp clientResponse
	// keep retrying if got a forced closed websocket conn and calling method
	// has retry annotation
	for attempt := 0; true; attempt++ {
		resp = fn.client.sendRequest(ctx, req, chCtor)
		if xerrors.Is(resp.Error, NetError) && fn.retry {
			waitTime := fn.backoff.next(attempt)
			log.Errorf("wait %s retry to sendrequest method %s %s", waitTime, req.Method, resp.Error)
			time.Sleep(waitTime)
			continue
		}

		if !fn.notify && resp.ID != req.ID {
			return fn.processError(xerrors.New("request and response id didn't match"))
		}

		if fn.valOut != -1 && !fn.returnValueIsChannel {
			val := reflect.New(fn.ftyp.Out(fn.valOut))

			if resp.Result != nil {
				log.Debugw("rpc result", "type", fn.ftyp.Out(fn.valOut))
				if err := json.Unmarshal(resp.Result, val.Interface()); err != nil {
					log.Warnw("unmarshaling failed", "message", string(resp.Result))
					return fn.processError(xerrors.Errorf("unmarshaling result: %w", err))
				}
			}

			retVal = func() reflect.Value { return val.Elem() }
		}
		break
	}

	return fn.processResponse(resp, retVal())
}

const (
	ProxyTagRetry     = "retry"
	ProxyTagNotify    = "notify"
	ProxyTagRPCMethod = "rpc_method"
)

func (c *client) makeRpcFunc(f reflect.StructField) (reflect.Value, error) {
	ftyp := f.Type
	if ftyp.Kind() != reflect.Func {
		return reflect.Value{}, xerrors.New("handler field not a func")
	}

	name := c.namespace + "." + f.Name
	if tag, ok := f.Tag.Lookup(ProxyTagRPCMethod); ok {
		name = tag
	}

	retry := c.retry
	if val, ok := f.Tag.Lookup("retry"); ok {
		retry = val == "true" //cover retry if has this tag
	}

	fun := &rpcFunc{
		client:  c,
		ftyp:    ftyp,
		name:    name,
		notify:  f.Tag.Get(ProxyTagNotify) == "true",
		retry:   retry,
		backoff: c.backoff,
	}
	fun.valOut, fun.errOut, fun.nout = processFuncOut(ftyp)

	if fun.valOut != -1 && fun.notify {
		return reflect.Value{}, xerrors.New("notify methods cannot return values")
	}

	fun.returnValueIsChannel = fun.valOut != -1 && ftyp.Out(fun.valOut).Kind() == reflect.Chan

	if ftyp.NumIn() > 0 && ftyp.In(0) == contextType {
		fun.hasCtx = 1
	}
	// note: hasCtx is also the number of the first non-context argument
	if ftyp.NumIn() > fun.hasCtx && ftyp.In(fun.hasCtx) == rtRawParams {
		if ftyp.NumIn() > fun.hasCtx+1 {
			return reflect.Value{}, xerrors.New("raw params can't be mixed with other arguments")
		}
		fun.hasRawParams = true
	}

	return reflect.MakeFunc(ftyp, fun.handleRpcCall), nil
}
