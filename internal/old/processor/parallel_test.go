package processor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/manager/mock"
	"github.com/benthosdev/benthos/v4/internal/message"
)

func TestParallelBasic(t *testing.T) {
	wg := sync.WaitGroup{}
	wg.Add(5)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Done()
		wg.Wait()
		_, _ = w.Write([]byte("foobar"))
	}))
	defer ts.Close()

	httpConf := NewConfig()
	httpConf.Type = TypeHTTP
	httpConf.HTTP.URL = ts.URL + "/testpost"
	conf := NewConfig()
	conf.Type = "parallel"
	conf.Parallel.Processors = []Config{httpConf}

	h, err := New(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	msgs, res := h.ProcessMessage(message.QuickBatch([][]byte{
		[]byte("foo"),
		[]byte("bar"),
		[]byte("baz"),
		[]byte("qux"),
		[]byte("quz"),
	}))
	if res != nil {
		t.Error(res)
	} else if expC, actC := 5, msgs[0].Len(); actC != expC {
		t.Errorf("Wrong result count: %v != %v", actC, expC)
	} else if exp, act := "foobar", string(message.GetAllBytes(msgs[0])[0]); act != exp {
		t.Errorf("Wrong result: %v != %v", act, exp)
	}
}

func TestParallelError(t *testing.T) {
	wg := sync.WaitGroup{}
	wg.Add(5)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Done()
		wg.Wait()
		reqBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(reqBytes) == "baz" {
			http.Error(w, "test error", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("foobar"))
	}))
	defer ts.Close()

	httpConf := NewConfig()
	httpConf.Type = TypeHTTP
	httpConf.HTTP.URL = ts.URL + "/testpost"
	httpConf.HTTP.NumRetries = 0

	conf := NewConfig()
	conf.Type = "parallel"
	conf.Parallel.Processors = []Config{httpConf}

	h, err := New(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	msgs, res := h.ProcessMessage(message.QuickBatch([][]byte{
		[]byte("foo"),
		[]byte("bar"),
		[]byte("baz"),
		[]byte("qux"),
		[]byte("quz"),
	}))
	if res != nil {
		t.Error(res)
	}
	if expC, actC := 5, msgs[0].Len(); actC != expC {
		t.Fatalf("Wrong result count: %v != %v", actC, expC)
	}
	if exp, act := "baz", string(msgs[0].Get(2).Get()); act != exp {
		t.Errorf("Wrong result: %v != %v", act, exp)
	}
	if !HasFailed(msgs[0].Get(2)) {
		t.Error("Expected failed flag")
	}
	for _, i := range []int{0, 1, 3, 4} {
		if exp, act := "foobar", string(msgs[0].Get(i).Get()); act != exp {
			t.Errorf("Wrong result: %v != %v", act, exp)
		}
		if HasFailed(msgs[0].Get(i)) {
			t.Error("Did not expect failed flag")
		}
	}
}

func TestParallelCapped(t *testing.T) {
	var reqs int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if req := atomic.AddInt64(&reqs, 1); req > 5 {
			t.Errorf("Beyond parallelism cap: %v", req)
		}
		<-time.After(time.Millisecond * 10)
		_, _ = w.Write([]byte("foobar"))
		atomic.AddInt64(&reqs, -1)
	}))
	defer ts.Close()

	httpConf := NewConfig()
	httpConf.Type = TypeHTTP
	httpConf.HTTP.URL = ts.URL + "/testpost"

	conf := NewConfig()
	conf.Type = "parallel"
	conf.Parallel.Processors = []Config{httpConf}
	conf.Parallel.Cap = 5

	h, err := New(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	msgs, res := h.ProcessMessage(message.QuickBatch([][]byte{
		[]byte("foo"),
		[]byte("bar"),
		[]byte("baz"),
		[]byte("qux"),
		[]byte("quz"),
		[]byte("foo2"),
		[]byte("bar2"),
		[]byte("baz2"),
		[]byte("qux2"),
		[]byte("quz2"),
	}))
	if res != nil {
		t.Error(res)
	} else if expC, actC := 10, msgs[0].Len(); actC != expC {
		t.Errorf("Wrong result count: %v != %v", actC, expC)
	} else if exp, act := "foobar", string(message.GetAllBytes(msgs[0])[0]); act != exp {
		t.Errorf("Wrong result: %v != %v", act, exp)
	}
}
