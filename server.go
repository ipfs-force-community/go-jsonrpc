package jsonrpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/gorilla/websocket"
)

// RPCServer provides a jsonrpc 2.0 http server handler
type RPCServer struct {
	*handler
	reverseClientBuilder func(context.Context, *wsConn) (context.Context, error)

	timeout      time.Duration
	pingInterval time.Duration
}

// NewServer creates new RPCServer instance
func NewServer(opts ...ServerOption) *RPCServer {
	config := defaultServerConfig()
	for _, o := range opts {
		o(&config)
	}

	return &RPCServer{
		handler:              makeHandler(config),
		reverseClientBuilder: config.reverseClientBuilder,
		timeout:              config.timeout,
		pingInterval:         config.pingInterval,
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (s *RPCServer) handleWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// TODO: allow setting
	// (note that we still are mostly covered by jwt tokens)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Header.Get("Sec-WebSocket-Protocol") != "" {
		w.Header().Set("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error(err)
		w.WriteHeader(500)
		return
	}

	wc := &wsConn{
		conn:         c,
		handler:      s,
		exiting:      make(chan struct{}),
		timeout:      s.timeout,
		pingInterval: s.pingInterval,
	}
	if s.reverseClientBuilder != nil {
		ctx, err = s.reverseClientBuilder(ctx, wc)
		if err != nil {
			log.Errorf("failed to build reverse client: %s", err)
			w.WriteHeader(500)
			return
		}
	}

	wc.handleWsConn(ctx)

	if err := c.Close(); err != nil {
		log.Error(err)
		return
	}
}

// TODO: return errors to clients per spec
func (s *RPCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	h := strings.ToLower(r.Header.Get("Connection"))
	if strings.Contains(h, "upgrade") {
		s.handleWS(ctx, w, r)
		return
	}

	s.handleReader(ctx, r.Body, w, rpcError)
}

func rpcError(wf func(func(io.Writer)), req *request, err error) {
	log.Errorf("RPC Error: %s", err)
	wf(func(w io.Writer) {
		if hw, ok := w.(http.ResponseWriter); ok {
			hw.WriteHeader(500)
		}

		log.Warnf("rpc error: %s", err)

		if req.ID == nil { // notification
			return
		}

		var code ErrorCode
		_ = xerrors.As(err, &code)
		resp := response{
			Jsonrpc: "2.0",
			ID:      req.ID,
			Error: respError{
				Code:    code,
				Message: err.Error(),
			},
		}

		err = json.NewEncoder(w).Encode(resp)
		if err != nil {
			log.Warnf("failed to write rpc error: %s", err)
			return
		}
	})
}

// Register registers new RPC handler
//
// Handler is any value with methods defined
func (s *RPCServer) Register(namespace string, handler interface{}) {
	s.register(namespace, handler)
}

func (s *RPCServer) AliasMethod(alias, original string) {
	s.aliasedMethods[alias] = original
}

var _ error = &respError{}
