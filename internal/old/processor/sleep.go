package processor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/benthosdev/benthos/v4/internal/bloblang/field"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/component/processor"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/interop"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	"github.com/benthosdev/benthos/v4/internal/tracing"
)

func init() {
	Constructors[TypeSleep] = TypeSpec{
		constructor: func(conf Config, mgr interop.Manager, log log.Modular, stats metrics.Type) (processor.V1, error) {
			p, err := newSleep(conf.Sleep, mgr)
			if err != nil {
				return nil, err
			}
			return processor.NewV2BatchedToV1Processor("sleep", p, mgr.Metrics()), nil
		},
		Categories: []string{
			"Utility",
		},
		Summary: `Sleep for a period of time specified as a duration string for each message. This processor will interpolate functions within the ` + "`duration`" + ` field, you can find a list of functions [here](/docs/configuration/interpolation#bloblang-queries).`,
		Config: docs.FieldComponent().WithChildren(
			docs.FieldInterpolatedString("duration", "The duration of time to sleep for each execution."),
		),
	}
}

// SleepConfig contains configuration fields for the Sleep processor.
type SleepConfig struct {
	Duration string `json:"duration" yaml:"duration"`
}

// NewSleepConfig returns a SleepConfig with default values.
func NewSleepConfig() SleepConfig {
	return SleepConfig{
		Duration: "",
	}
}

//------------------------------------------------------------------------------

type sleepProc struct {
	closeOnce   sync.Once
	closeChan   chan struct{}
	durationStr *field.Expression
	log         log.Modular
}

func newSleep(conf SleepConfig, mgr interop.Manager) (*sleepProc, error) {
	durationStr, err := mgr.BloblEnvironment().NewField(conf.Duration)
	if err != nil {
		return nil, fmt.Errorf("failed to parse duration expression: %v", err)
	}
	t := &sleepProc{
		closeChan:   make(chan struct{}),
		durationStr: durationStr,
		log:         mgr.Logger(),
	}
	return t, nil
}

func (s *sleepProc) ProcessBatch(ctx context.Context, spans []*tracing.Span, msg *message.Batch) ([]*message.Batch, error) {
	_ = msg.Iter(func(i int, p *message.Part) error {
		period, err := time.ParseDuration(s.durationStr.String(i, msg))
		if err != nil {
			s.log.Errorf("Failed to parse duration: %v\n", err)
			return nil
		}
		select {
		case <-time.After(period):
		case <-ctx.Done():
			return errors.New("stop")
		case <-s.closeChan:
			return errors.New("stop")
		}
		return nil
	})
	return []*message.Batch{msg}, nil
}

func (s *sleepProc) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		close(s.closeChan)
	})
	return nil
}
