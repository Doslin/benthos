package input

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/benthosdev/benthos/v4/internal/codec"
	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/input"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/interop"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	"github.com/benthosdev/benthos/v4/internal/old/input/reader"
)

//------------------------------------------------------------------------------

func init() {
	Constructors[TypeSocket] = TypeSpec{
		constructor: fromSimpleConstructor(NewSocket),
		Summary: `
Connects to a tcp or unix socket and consumes a continuous stream of messages.`,
		Config: docs.FieldComponent().WithChildren(
			docs.FieldString("network", "A network type to assume (unix|tcp).").HasOptions(
				"unix", "tcp",
			),
			docs.FieldString("address", "The address to connect to.", "/tmp/benthos.sock", "127.0.0.1:6000"),
			codec.ReaderDocs.AtVersion("3.42.0"),
			docs.FieldInt("max_buffer", "The maximum message buffer size. Must exceed the largest message to be consumed.").Advanced(),
		),
		Categories: []string{
			"Network",
		},
	}
}

//------------------------------------------------------------------------------

// SocketConfig contains configuration values for the Socket input type.
type SocketConfig struct {
	Network   string `json:"network" yaml:"network"`
	Address   string `json:"address" yaml:"address"`
	Codec     string `json:"codec" yaml:"codec"`
	MaxBuffer int    `json:"max_buffer" yaml:"max_buffer"`
}

// NewSocketConfig creates a new SocketConfig with default values.
func NewSocketConfig() SocketConfig {
	return SocketConfig{
		Network:   "",
		Address:   "",
		Codec:     "lines",
		MaxBuffer: 1000000,
	}
}

//------------------------------------------------------------------------------

// NewSocket creates a new Socket input type.
func NewSocket(conf Config, mgr interop.Manager, log log.Modular, stats metrics.Type) (input.Streamed, error) {
	rdr, err := newSocketClient(conf.Socket, log)
	if err != nil {
		return nil, err
	}
	// TODO: Consider removing the async cut off here. It adds an overhead and
	// we can get the same results by making sure that the async readers forward
	// CloseAsync all the way through. We would need it to be configurable as it
	// wouldn't be appropriate for inputs that have real acks.
	return NewAsyncReader(TypeSocket, true, reader.NewAsyncCutOff(reader.NewAsyncPreserver(rdr)), log, stats)
}

//------------------------------------------------------------------------------

type socketClient struct {
	log log.Modular

	conf      SocketConfig
	codecCtor codec.ReaderConstructor

	codecMut sync.Mutex
	codec    codec.Reader
}

func newSocketClient(conf SocketConfig, logger log.Modular) (*socketClient, error) {
	switch conf.Network {
	case "tcp", "unix":
	default:
		return nil, fmt.Errorf("socket network '%v' is not supported by this input", conf.Network)
	}

	codecConf := codec.NewReaderConfig()
	codecConf.MaxScanTokenSize = conf.MaxBuffer
	ctor, err := codec.GetReader(conf.Codec, codecConf)
	if err != nil {
		return nil, err
	}

	return &socketClient{
		log:       logger,
		conf:      conf,
		codecCtor: ctor,
	}, nil
}

// ConnectWithContext attempts to establish a connection to the target S3 bucket
// and any relevant queues used to traverse the objects (SQS, etc).
func (s *socketClient) ConnectWithContext(ctx context.Context) error {
	s.codecMut.Lock()
	defer s.codecMut.Unlock()

	if s.codec != nil {
		return nil
	}

	conn, err := net.Dial(s.conf.Network, s.conf.Address)
	if err != nil {
		return err
	}

	if s.codec, err = s.codecCtor("", conn, func(ctx context.Context, err error) error {
		return nil
	}); err != nil {
		conn.Close()
		return err
	}

	s.log.Infof("Consuming from socket at '%v://%v'\n", s.conf.Network, s.conf.Address)
	return nil
}

// ReadWithContext attempts to read a new message from the target S3 bucket.
func (s *socketClient) ReadWithContext(ctx context.Context) (*message.Batch, reader.AsyncAckFn, error) {
	s.codecMut.Lock()
	codec := s.codec
	s.codecMut.Unlock()

	if codec == nil {
		return nil, nil, component.ErrNotConnected
	}

	parts, codecAckFn, err := codec.Next(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			err = component.ErrTimeout
		}
		if err != component.ErrTimeout {
			s.codecMut.Lock()
			if s.codec != nil && s.codec == codec {
				s.codec.Close(ctx)
				s.codec = nil
			}
			s.codecMut.Unlock()
		}
		if errors.Is(err, io.EOF) {
			return nil, nil, component.ErrTimeout
		}
		return nil, nil, err
	}

	// We simply bounce rejected messages in a loop downstream so there's no
	// benefit to aggregating acks.
	_ = codecAckFn(context.Background(), nil)

	msg := message.QuickBatch(nil)
	msg.Append(parts...)

	if msg.Len() == 0 {
		return nil, nil, component.ErrTimeout
	}

	return msg, func(rctx context.Context, res error) error {
		return nil
	}, nil
}

// CloseAsync begins cleaning up resources used by this reader asynchronously.
func (s *socketClient) CloseAsync() {
	s.codecMut.Lock()
	if s.codec != nil {
		s.codec.Close(context.Background())
		s.codec = nil
	}
	s.codecMut.Unlock()
}

// WaitForClose will block until either the reader is closed or a specified
// timeout occurs.
func (s *socketClient) WaitForClose(time.Duration) error {
	return nil
}
