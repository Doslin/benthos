package writer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v7"

	ibatch "github.com/benthosdev/benthos/v4/internal/batch"
	"github.com/benthosdev/benthos/v4/internal/batch/policy"
	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	bredis "github.com/benthosdev/benthos/v4/internal/impl/redis/old"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	"github.com/benthosdev/benthos/v4/internal/metadata"
)

//------------------------------------------------------------------------------

// RedisStreamsConfig contains configuration fields for the RedisStreams output type.
type RedisStreamsConfig struct {
	bredis.Config `json:",inline" yaml:",inline"`
	Stream        string                       `json:"stream" yaml:"stream"`
	BodyKey       string                       `json:"body_key" yaml:"body_key"`
	MaxLenApprox  int64                        `json:"max_length" yaml:"max_length"`
	MaxInFlight   int                          `json:"max_in_flight" yaml:"max_in_flight"`
	Metadata      metadata.ExcludeFilterConfig `json:"metadata" yaml:"metadata"`
	Batching      policy.Config                `json:"batching" yaml:"batching"`
}

// NewRedisStreamsConfig creates a new RedisStreamsConfig with default values.
func NewRedisStreamsConfig() RedisStreamsConfig {
	return RedisStreamsConfig{
		Config:       bredis.NewConfig(),
		Stream:       "",
		BodyKey:      "body",
		MaxLenApprox: 0,
		MaxInFlight:  64,
		Metadata:     metadata.NewExcludeFilterConfig(),
		Batching:     policy.NewConfig(),
	}
}

//------------------------------------------------------------------------------

// RedisStreams is an output type that serves RedisStreams messages.
type RedisStreams struct {
	log   log.Modular
	stats metrics.Type

	conf       RedisStreamsConfig
	metaFilter *metadata.ExcludeFilter

	client  redis.UniversalClient
	connMut sync.RWMutex
}

// NewRedisStreams creates a new RedisStreams output type.
func NewRedisStreams(
	conf RedisStreamsConfig,
	log log.Modular,
	stats metrics.Type,
) (*RedisStreams, error) {

	r := &RedisStreams{
		log:   log,
		stats: stats,
		conf:  conf,
	}

	var err error
	if r.metaFilter, err = conf.Metadata.Filter(); err != nil {
		return nil, fmt.Errorf("failed to construct metadata filter: %w", err)
	}

	if _, err = conf.Config.Client(); err != nil {
		return nil, err
	}
	return r, nil
}

//------------------------------------------------------------------------------

// ConnectWithContext establishes a connection to an RedisStreams server.
func (r *RedisStreams) ConnectWithContext(ctx context.Context) error {
	return r.Connect()
}

// Connect establishes a connection to an RedisStreams server.
func (r *RedisStreams) Connect() error {
	r.connMut.Lock()
	defer r.connMut.Unlock()

	client, err := r.conf.Config.Client()
	if err != nil {
		return err
	}
	if _, err = client.Ping().Result(); err != nil {
		return err
	}

	r.log.Infof("Pushing messages to Redis stream: %v\n", r.conf.Stream)

	r.client = client
	return nil
}

//------------------------------------------------------------------------------

// WriteWithContext attempts to write a message by pushing it to a Redis stream.
func (r *RedisStreams) WriteWithContext(ctx context.Context, msg *message.Batch) error {
	return r.Write(msg)
}

// Write attempts to write a message by pushing it to a Redis stream.
func (r *RedisStreams) Write(msg *message.Batch) error {
	r.connMut.RLock()
	client := r.client
	r.connMut.RUnlock()

	if client == nil {
		return component.ErrNotConnected
	}

	partToMap := func(p *message.Part) map[string]interface{} {
		values := map[string]interface{}{}
		_ = r.metaFilter.Iter(p, func(k, v string) error {
			values[k] = v
			return nil
		})
		values[r.conf.BodyKey] = p.Get()
		return values
	}

	if msg.Len() == 1 {
		if err := client.XAdd(&redis.XAddArgs{
			ID:           "*",
			Stream:       r.conf.Stream,
			MaxLenApprox: r.conf.MaxLenApprox,
			Values:       partToMap(msg.Get(0)),
		}).Err(); err != nil {
			_ = r.disconnect()
			r.log.Errorf("Error from redis: %v\n", err)
			return component.ErrNotConnected
		}
		return nil
	}

	pipe := client.Pipeline()
	_ = msg.Iter(func(i int, p *message.Part) error {
		_ = pipe.XAdd(&redis.XAddArgs{
			ID:           "*",
			Stream:       r.conf.Stream,
			MaxLenApprox: r.conf.MaxLenApprox,
			Values:       partToMap(p),
		})
		return nil
	})
	cmders, err := pipe.Exec()
	if err != nil {
		_ = r.disconnect()
		r.log.Errorf("Error from redis: %v\n", err)
		return component.ErrNotConnected
	}

	var batchErr *ibatch.Error
	for i, res := range cmders {
		if res.Err() != nil {
			if batchErr == nil {
				batchErr = ibatch.NewError(msg, res.Err())
			}
			batchErr.Failed(i, res.Err())
		}
	}
	if batchErr != nil {
		return batchErr
	}
	return nil
}

// disconnect safely closes a connection to an RedisStreams server.
func (r *RedisStreams) disconnect() error {
	r.connMut.Lock()
	defer r.connMut.Unlock()
	if r.client != nil {
		err := r.client.Close()
		r.client = nil
		return err
	}
	return nil
}

// CloseAsync shuts down the RedisStreams output and stops processing messages.
func (r *RedisStreams) CloseAsync() {
	_ = r.disconnect()
}

// WaitForClose blocks until the RedisStreams output has closed down.
func (r *RedisStreams) WaitForClose(timeout time.Duration) error {
	return nil
}

//------------------------------------------------------------------------------
