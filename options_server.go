package jsonrpc

import (
	"context"
	"reflect"
	"time"

	"golang.org/x/xerrors"
)

// note: we embed reflect.Type because proxy-structs are not comparable
type jsonrpcReverseClient struct{ reflect.Type }

type ParamDecoder func(ctx context.Context, json []byte) (reflect.Value, error)

// Register rpcHandler bind func-field or method in struct
type ProxyBind = int

const (
	PBMethod = 0
	PBField  = 1
)

type ServerConfig struct {
	paramDecoders  map[reflect.Type]ParamDecoder
	maxRequestSize int64
	timeout        time.Duration
	pingInterval   time.Duration
	proxyBind      ProxyBind

	reverseClientBuilder func(context.Context, *wsConn) (context.Context, error)
}

type ServerOption func(c *ServerConfig)

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		paramDecoders:  map[reflect.Type]ParamDecoder{},
		maxRequestSize: DEFAULT_MAX_REQUEST_SIZE,
		timeout:        time.Second * 30,
		pingInterval:   time.Second * 5,
	}
}

func WithParamDecoder(t interface{}, decoder ParamDecoder) ServerOption {
	return func(c *ServerConfig) {
		c.paramDecoders[reflect.TypeOf(t).Elem()] = decoder
	}
}

func WithMaxRequestSize(max int64) ServerOption {
	return func(c *ServerConfig) {
		c.maxRequestSize = max
	}
}

func WithServerTimeout(timeout time.Duration) ServerOption {
	return func(c *ServerConfig) {
		c.timeout = timeout
	}
}

func WithProxyBind(pb ProxyBind) ServerOption {
	return func(c *ServerConfig) {
		c.proxyBind = pb
	}
}

// WithReverseClient will allow extracting reverse client on **WEBSOCKET** calls.
// RP is a proxy-struct type, much like the one passed to NewClient.
func WithReverseClient[RP any](namespace string) ServerOption {
	return func(c *ServerConfig) {
		c.reverseClientBuilder = func(ctx context.Context, conn *wsConn) (context.Context, error) {
			cl := client{
				namespace:     namespace,
				paramEncoders: map[reflect.Type]ParamEncoder{},
			}

			cl.exiting = conn.exiting

			requests := cl.setupRequestChan()
			conn.requests = requests

			calls := new(RP)

			err := cl.provide([]interface{}{
				calls,
			})
			if err != nil {
				return nil, xerrors.Errorf("provide reverse client calls: %w", err)
			}

			return context.WithValue(ctx, jsonrpcReverseClient{reflect.TypeOf(calls).Elem()}, calls), nil
		}
	}
}

// ExtractReverseClient will extract reverse client from context. Reverse client for the type
// will only be present if the server was constructed with a matching WithReverseClient option
// and the connection was a websocket connection.
// If there is no reverse client, the call will return a zero value and `false`. Otherwise a reverse
// client and `true` will be returned.
func ExtractReverseClient[C any](ctx context.Context) (C, bool) {
	c, ok := ctx.Value(jsonrpcReverseClient{reflect.TypeOf(new(C)).Elem()}).(*C)
	if !ok {
		return *new(C), false
	}
	if c == nil {
		// something is very wrong, but don't panic
		return *new(C), false
	}

	return *c, ok
}
