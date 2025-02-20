package output

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/manager/mock"
	"github.com/benthosdev/benthos/v4/internal/message"
)

func TestHTTPClientMultipartEnabled(t *testing.T) {
	resultChan := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(mediaType, "multipart/"))

		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)

			msgBytes, err := io.ReadAll(p)
			require.NoError(t, err)

			resultChan <- string(msgBytes)
		}
	}))
	defer ts.Close()

	conf := NewConfig()
	conf.Type = TypeHTTPClient
	conf.HTTPClient.BatchAsMultipart = true
	conf.HTTPClient.URL = ts.URL + "/testpost"

	h, err := NewHTTPClient(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	require.NoError(t, err)

	tChan := make(chan message.Transaction)
	require.NoError(t, h.Consume(tChan))

	resChan := make(chan error)
	select {
	case tChan <- message.NewTransaction(message.QuickBatch([][]byte{
		[]byte("PART-A"),
		[]byte("PART-B"),
		[]byte("PART-C"),
	}), resChan):
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	for _, exp := range []string{
		"PART-A",
		"PART-B",
		"PART-C",
	} {
		select {
		case resMsg := <-resultChan:
			assert.Equal(t, exp, resMsg)
		case <-time.After(time.Second):
			t.Fatal("Action timed out")
		}
	}

	select {
	case res := <-resChan:
		assert.NoError(t, res)
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	h.CloseAsync()
	require.NoError(t, h.WaitForClose(time.Second))
}

func TestHTTPClientMultipartDisabled(t *testing.T) {
	resultChan := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resBytes, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		resultChan <- string(resBytes)
	}))
	defer ts.Close()

	conf := NewConfig()
	conf.Type = TypeHTTPClient
	conf.HTTPClient.URL = ts.URL + "/testpost"
	conf.HTTPClient.BatchAsMultipart = false
	conf.HTTPClient.MaxInFlight = 1

	h, err := NewHTTPClient(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	require.NoError(t, err)

	tChan := make(chan message.Transaction)
	require.NoError(t, h.Consume(tChan))

	resChan := make(chan error)
	select {
	case tChan <- message.NewTransaction(message.QuickBatch([][]byte{
		[]byte("PART-A"),
		[]byte("PART-B"),
		[]byte("PART-C"),
	}), resChan):
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	for _, exp := range []string{
		"PART-A",
		"PART-B",
		"PART-C",
	} {
		select {
		case resMsg := <-resultChan:
			assert.Equal(t, exp, resMsg)
		case <-time.After(time.Second):
			t.Fatal("Action timed out")
		}
	}

	select {
	case res := <-resChan:
		assert.NoError(t, res)
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	h.CloseAsync()
	require.NoError(t, h.WaitForClose(time.Second))
}
