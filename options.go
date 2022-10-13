package jsonrpc

import (
	"reflect"
	"time"

	"github.com/gorilla/websocket"
)

const (
	methodMinRetryDelay = 100 * time.Millisecond
	methodMaxRetryDelay = 10 * time.Minute
)

type ParamEncoder func(reflect.Value) (reflect.Value, error)

type Config struct {
	retryBackoff backoff
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration
	retry 			 bool

	paramEncoders map[reflect.Type]ParamEncoder
	errors        *Errors

	noReconnect      bool
	proxyConnFactory func(func() (*websocket.Conn, error)) func() (*websocket.Conn, error) // for testing
}

func defaultConfig() Config {
	return Config{
		reconnectBackoff: backoff{
			minDelay: 100 * time.Millisecond,
			maxDelay: 5 * time.Second,
		},
		retryBackoff: backoff{
			minDelay: methodMinRetryDelay,
			maxDelay: methodMaxRetryDelay,
		},
		pingInterval: 5 * time.Second,
		timeout:      30 * time.Second,

		paramEncoders: map[reflect.Type]ParamEncoder{},
	}
}

type Option func(c *Config)

func WithReconnectBackoff(minDelay, maxDelay time.Duration) func(c *Config) {
	return func(c *Config) {
		c.reconnectBackoff = backoff{
			minDelay: minDelay,
			maxDelay: maxDelay,
		}
	}
}

func WithRetryBackoff(minDelay, maxDelay time.Duration) func(c *Config) {
	return func(c *Config) {
		c.retryBackoff = backoff{
			minDelay: minDelay,
			maxDelay: maxDelay,
		}
	}
}

func WithRetry(d bool) func(c *Config) {
	return func(c *Config) {
		c.retry = d
	}
}

// Must be < Timeout/2
func WithPingInterval(d time.Duration) func(c *Config) {
	return func(c *Config) {
		c.pingInterval = d
	}
}

func WithTimeout(d time.Duration) func(c *Config) {
	return func(c *Config) {
		c.timeout = d
	}
}

func WithNoReconnect() func(c *Config) {
	return func(c *Config) {
		c.noReconnect = true
	}
}

func WithParamEncoder(t interface{}, encoder ParamEncoder) func(c *Config) {
	return func(c *Config) {
		c.paramEncoders[reflect.TypeOf(t).Elem()] = encoder
	}
}

func WithErrors(es Errors) func(c *Config) {
	return func(c *Config) {
		c.errors = &es
	}
}
