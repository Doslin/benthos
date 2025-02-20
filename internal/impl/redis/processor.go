package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-redis/redis/v7"

	"github.com/benthosdev/benthos/v4/internal/bloblang/field"
	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component/processor"
	"github.com/benthosdev/benthos/v4/internal/docs"
	bredis "github.com/benthosdev/benthos/v4/internal/impl/redis/old"
	"github.com/benthosdev/benthos/v4/internal/interop"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	oprocessor "github.com/benthosdev/benthos/v4/internal/old/processor"
	"github.com/benthosdev/benthos/v4/internal/tracing"
)

//------------------------------------------------------------------------------

func init() {
	err := bundle.AllProcessors.Add(func(conf oprocessor.Config, mgr bundle.NewManagement) (processor.V1, error) {
		p, err := newRedisProc(conf.Redis, mgr)
		if err != nil {
			return nil, err
		}
		return processor.NewV2BatchedToV1Processor("redis", p, mgr.Metrics()), nil
	}, docs.ComponentSpec{
		Name:   "redis",
		Type:   docs.TypeProcessor,
		Status: docs.StatusStable,
		Categories: []string{
			"Integration",
		},
		Summary: `
Performs actions against Redis that aren't possible using a
` + "[`cache`](/docs/components/processors/cache)" + ` processor. Actions are
performed for each message of a batch, where the contents are replaced with the
result.`,
		Description: `
## Operators

### ` + "`keys`" + `

Returns an array of strings containing all the keys that match the pattern specified by the ` + "`key` field" + `.

### ` + "`scard`" + `

Returns the cardinality of a set, or ` + "`0`" + ` if the key does not exist.

### ` + "`sadd`" + `

Adds a new member to a set. Returns ` + "`1`" + ` if the member was added.

### ` + "`incrby`" + `

Increments the number stored at ` + "`key`" + ` by the message content. If the
key does not exist, it is set to ` + "`0`" + ` before performing the operation.
Returns the value of ` + "`key`" + ` after the increment.`,
		Config: docs.FieldComponent().WithChildren(
			bredis.ConfigDocs().Add(
				docs.FieldString("operator", "The [operator](#operators) to apply.").HasOptions("scard", "sadd", "incrby", "keys").HasDefault(""),
				docs.FieldString("key", "A key to use for the target operator.").IsInterpolated().HasDefault(""),
				docs.FieldInt("retries", "The maximum number of retries before abandoning a request.").Advanced().HasDefault(3),
				docs.FieldString("retry_period", "The time to wait before consecutive retry attempts.").Advanced().HasDefault("500ms"),
			)...,
		),
		Examples: []docs.AnnotatedExample{
			{
				Title: "Querying Cardinality",
				Summary: `
If given payloads containing a metadata field ` + "`set_key`" + ` it's possible
to query and store the cardinality of the set for each message using a
` + "[`branch` processor](/docs/components/processors/branch)" + ` in order to
augment rather than replace the message contents:`,
				Config: `
pipeline:
  processors:
    - branch:
        processors:
          - redis:
              url: TODO
              operator: scard
              key: ${! meta("set_key") }
        result_map: 'root.cardinality = this'
`,
			},
			{
				Title: "Running Total",
				Summary: `
If we have JSON data containing number of friends visited during covid 19:

` + "```json" + `
{"name":"ash","month":"feb","year":2019,"friends_visited":10}
{"name":"ash","month":"apr","year":2019,"friends_visited":-2}
{"name":"bob","month":"feb","year":2019,"friends_visited":3}
{"name":"bob","month":"apr","year":2019,"friends_visited":1}
` + "```" + `

We can add a field that contains the running total number of friends visited:

` + "```json" + `
{"name":"ash","month":"feb","year":2019,"friends_visited":10,"total":10}
{"name":"ash","month":"apr","year":2019,"friends_visited":-2,"total":8}
{"name":"bob","month":"feb","year":2019,"friends_visited":3,"total":3}
{"name":"bob","month":"apr","year":2019,"friends_visited":1,"total":4}
` + "```" + `

Using the ` + "`incrby`" + ` operator:
                `,
				Config: `
pipeline:
  processors:
    - branch:
        request_map: |
            root = this.friends_visited
            meta name = this.name
        processors:
          - redis:
              url: TODO
              operator: incrby
              key: ${! meta("name") }
        result_map: 'root.total = this'
`,
			},
		},
	})
	if err != nil {
		panic(err)
	}
}

//------------------------------------------------------------------------------

type redisProc struct {
	log log.Modular
	key *field.Expression

	operator    redisOperator
	client      redis.UniversalClient
	retries     int
	retryPeriod time.Duration
}

func newRedisProc(conf oprocessor.RedisConfig, mgr interop.Manager) (*redisProc, error) {
	var retryPeriod time.Duration
	if tout := conf.RetryPeriod; len(tout) > 0 {
		var err error
		if retryPeriod, err = time.ParseDuration(tout); err != nil {
			return nil, fmt.Errorf("failed to parse retry period string: %v", err)
		}
	}

	client, err := conf.Config.Client()
	if err != nil {
		return nil, err
	}

	key, err := mgr.BloblEnvironment().NewField(conf.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key expression: %v", err)
	}

	r := &redisProc{
		log: mgr.Logger(),
		key: key,

		retries:     conf.Retries,
		retryPeriod: retryPeriod,
		client:      client,
	}

	if r.operator, err = getRedisOperator(conf.Operator); err != nil {
		return nil, err
	}
	return r, nil
}

type redisOperator func(r *redisProc, key string, part *message.Part) error

func newRedisKeysOperator() redisOperator {
	return func(r *redisProc, key string, part *message.Part) error {
		res, err := r.client.Keys(key).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("Keys command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.Keys(key).Result()
		}
		if err != nil {
			return err
		}

		iRes := make([]interface{}, 0, len(res))
		for _, v := range res {
			iRes = append(iRes, v)
		}
		part.SetJSON(iRes)
		return nil
	}
}

func newRedisSCardOperator() redisOperator {
	return func(r *redisProc, key string, part *message.Part) error {
		res, err := r.client.SCard(key).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("SCard command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.SCard(key).Result()
		}
		if err != nil {
			return err
		}

		part.Set(strconv.AppendInt(nil, res, 10))
		return nil
	}
}

func newRedisSAddOperator() redisOperator {
	return func(r *redisProc, key string, part *message.Part) error {
		res, err := r.client.SAdd(key, part.Get()).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("SAdd command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.SAdd(key, part.Get()).Result()
		}
		if err != nil {
			return err
		}

		part.Set(strconv.AppendInt(nil, res, 10))
		return nil
	}
}

func newRedisIncrByOperator() redisOperator {
	return func(r *redisProc, key string, part *message.Part) error {
		valueInt, err := strconv.Atoi(string(part.Get()))
		if err != nil {
			return err
		}
		res, err := r.client.IncrBy(key, int64(valueInt)).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("incrby command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.IncrBy(key, int64(valueInt)).Result()
		}
		if err != nil {
			return err
		}

		part.Set(strconv.AppendInt(nil, res, 10))
		return nil
	}
}

func getRedisOperator(opStr string) (redisOperator, error) {
	switch opStr {
	case "keys":
		return newRedisKeysOperator(), nil
	case "sadd":
		return newRedisSAddOperator(), nil
	case "scard":
		return newRedisSCardOperator(), nil
	case "incrby":
		return newRedisIncrByOperator(), nil
	}
	return nil, fmt.Errorf("operator not recognised: %v", opStr)
}

func (r *redisProc) ProcessBatch(ctx context.Context, spans []*tracing.Span, msg *message.Batch) ([]*message.Batch, error) {
	newMsg := msg.Copy()
	_ = newMsg.Iter(func(index int, part *message.Part) error {
		key := r.key.String(index, newMsg)
		if err := r.operator(r, key, part); err != nil {
			r.log.Debugf("Operator failed for key '%s': %v", key, err)
			return err
		}
		return nil
	})
	return []*message.Batch{newMsg}, nil
}

func (r *redisProc) Close(ctx context.Context) error {
	return r.client.Close()
}
