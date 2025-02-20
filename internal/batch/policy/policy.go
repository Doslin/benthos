package policy

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/benthosdev/benthos/v4/internal/bloblang/mapping"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	iprocessor "github.com/benthosdev/benthos/v4/internal/component/processor"
	"github.com/benthosdev/benthos/v4/internal/interop"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	"github.com/benthosdev/benthos/v4/internal/old/processor"
)

// Config contains configuration parameters for a batch policy.
type Config struct {
	ByteSize   int                `json:"byte_size" yaml:"byte_size"`
	Count      int                `json:"count" yaml:"count"`
	Check      string             `json:"check" yaml:"check"`
	Period     string             `json:"period" yaml:"period"`
	Processors []processor.Config `json:"processors" yaml:"processors"`
}

// NewConfig creates a default PolicyConfig.
func NewConfig() Config {
	return Config{
		ByteSize:   0,
		Count:      0,
		Check:      "",
		Period:     "",
		Processors: []processor.Config{},
	}
}

// IsNoop returns true if this batch policy configuration does nothing.
func (p Config) IsNoop() bool {
	if p.ByteSize > 0 {
		return false
	}
	if p.Count > 1 {
		return false
	}
	if len(p.Check) > 0 {
		return false
	}
	if len(p.Period) > 0 {
		return false
	}
	if len(p.Processors) > 0 {
		return false
	}
	return true
}

func (p Config) isLimited() bool {
	if p.ByteSize > 0 {
		return true
	}
	if p.Count > 0 {
		return true
	}
	if len(p.Period) > 0 {
		return true
	}
	if len(p.Check) > 0 {
		return true
	}
	return false
}

func (p Config) isHardLimited() bool {
	if p.ByteSize > 0 {
		return true
	}
	if p.Count > 0 {
		return true
	}
	if len(p.Period) > 0 {
		return true
	}
	return false
}

//------------------------------------------------------------------------------

// Batcher implements a batching policy by buffering messages until, based on a
// set of rules, the buffered messages are ready to be sent onwards as a batch.
type Batcher struct {
	log log.Modular

	byteSize  int
	count     int
	period    time.Duration
	check     *mapping.Executor
	procs     []iprocessor.V1
	sizeTally int
	parts     []*message.Part

	triggered bool
	lastBatch time.Time

	mSizeBatch   metrics.StatCounter
	mCountBatch  metrics.StatCounter
	mPeriodBatch metrics.StatCounter
	mCheckBatch  metrics.StatCounter
}

// New creates an empty policy with default rules.
func New(conf Config, mgr interop.Manager) (*Batcher, error) {
	if !conf.isLimited() {
		return nil, errors.New("batch policy must have at least one active trigger")
	}
	if !conf.isHardLimited() {
		mgr.Logger().Warnln("Batch policy should have at least one of count, period or byte_size set in order to provide a hard batch ceiling.")
	}
	var err error
	var check *mapping.Executor
	if len(conf.Check) > 0 {
		if check, err = mgr.BloblEnvironment().NewMapping(conf.Check); err != nil {
			return nil, fmt.Errorf("failed to parse check: %v", err)
		}
	}
	var period time.Duration
	if len(conf.Period) > 0 {
		if period, err = time.ParseDuration(conf.Period); err != nil {
			return nil, fmt.Errorf("failed to parse duration string: %v", err)
		}
	}
	var procs []iprocessor.V1
	for i, pconf := range conf.Processors {
		pMgr := mgr.IntoPath("processors", strconv.Itoa(i))
		proc, err := processor.New(pconf, pMgr, pMgr.Logger(), pMgr.Metrics())
		if err != nil {
			return nil, err
		}
		procs = append(procs, proc)
	}

	batchOn := mgr.Metrics().GetCounterVec("batch_created", "mechanism")
	return &Batcher{
		log: mgr.Logger(),

		byteSize: conf.ByteSize,
		count:    conf.Count,
		period:   period,
		check:    check,
		procs:    procs,

		lastBatch: time.Now(),

		mSizeBatch:   batchOn.With("size"),
		mCountBatch:  batchOn.With("count"),
		mPeriodBatch: batchOn.With("period"),
		mCheckBatch:  batchOn.With("check"),
	}, nil
}

//------------------------------------------------------------------------------

// Add a new message part to this batch policy. Returns true if this part
// triggers the conditions of the policy.
func (p *Batcher) Add(part *message.Part) bool {
	p.sizeTally += len(part.Get())
	p.parts = append(p.parts, part)

	if !p.triggered && p.count > 0 && len(p.parts) >= p.count {
		p.triggered = true
		p.mCountBatch.Incr(1)
		p.log.Traceln("Batching based on count")
	}
	if !p.triggered && p.byteSize > 0 && p.sizeTally >= p.byteSize {
		p.triggered = true
		p.mSizeBatch.Incr(1)
		p.log.Traceln("Batching based on byte_size")
	}
	if p.check != nil && !p.triggered {
		tmpMsg := message.QuickBatch(nil)
		tmpMsg.SetAll(p.parts)

		test, err := p.check.QueryPart(tmpMsg.Len()-1, tmpMsg)
		if err != nil {
			test = false
			p.log.Errorf("Failed to execute batch check query: %v\n", err)
		}
		if test {
			p.triggered = true
			p.mCheckBatch.Incr(1)
			p.log.Traceln("Batching based on check query")
		}
	}
	return p.triggered || (p.period > 0 && time.Since(p.lastBatch) > p.period)
}

// Flush clears all messages stored by this batch policy. Returns nil if the
// policy is currently empty.
func (p *Batcher) Flush() *message.Batch {
	var newMsg *message.Batch

	resultMsgs := p.flushAny()
	if len(resultMsgs) == 1 {
		newMsg = resultMsgs[0]
	} else if len(resultMsgs) > 1 {
		newMsg = message.QuickBatch(nil)
		var parts []*message.Part
		for _, m := range resultMsgs {
			_ = m.Iter(func(_ int, p *message.Part) error {
				parts = append(parts, p)
				return nil
			})
		}
		newMsg.SetAll(parts)
	}
	return newMsg
}

func (p *Batcher) flushAny() []*message.Batch {
	var newMsg *message.Batch
	if len(p.parts) > 0 {
		if !p.triggered && p.period > 0 && time.Since(p.lastBatch) > p.period {
			p.mPeriodBatch.Incr(1)
			p.log.Traceln("Batching based on period")
		}
		newMsg = message.QuickBatch(nil)
		newMsg.Append(p.parts...)
	}
	p.parts = nil
	p.sizeTally = 0
	p.lastBatch = time.Now()
	p.triggered = false

	if newMsg == nil {
		return nil
	}

	if len(p.procs) > 0 {
		resultMsgs, res := processor.ExecuteAll(p.procs, newMsg)
		if res != nil {
			p.log.Errorf("Batch processors resulted in error: %v, the batch has been dropped.", res)
			return nil
		}
		return resultMsgs
	}

	return []*message.Batch{newMsg}
}

// Count returns the number of currently buffered message parts within this
// policy.
func (p *Batcher) Count() int {
	return len(p.parts)
}

// UntilNext returns a duration indicating how long until the current batch
// should be flushed due to a configured period. A negative duration indicates
// a period has not been set.
func (p *Batcher) UntilNext() time.Duration {
	if p.period <= 0 {
		return -1
	}
	return time.Until(p.lastBatch.Add(p.period))
}

//------------------------------------------------------------------------------

// CloseAsync shuts down the policy resources.
func (p *Batcher) CloseAsync() {
	for _, c := range p.procs {
		c.CloseAsync()
	}
}

// WaitForClose blocks until the processor has closed down.
func (p *Batcher) WaitForClose(timeout time.Duration) error {
	stopBy := time.Now().Add(timeout)
	for _, c := range p.procs {
		if err := c.WaitForClose(time.Until(stopBy)); err != nil {
			return err
		}
	}
	return nil
}
