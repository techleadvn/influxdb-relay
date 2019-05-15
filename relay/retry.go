package relay

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	retryInitial    = 500 * time.Millisecond
	retryMultiplier = 2
)

// Operation -TODO-
type Operation func() error

// Buffers and retries operations, if the buffer is full operations are dropped.
// Only tries one operation at a time, the next operation is not attempted
// until success or timeout of the previous operation.
// There is no delay between attempts of different operations.
type retryBuffer struct {
	buffering int32
	flushing int32

	initialInterval time.Duration
	multiplier      time.Duration
	maxInterval     time.Duration

	maxBuffered int
	maxBatch    int

	list *bufferList

	p poster

	logger *log.Logger
}

func newRetryBuffer(size, batch int, max time.Duration, p poster) *retryBuffer {
	r := &retryBuffer{
		initialInterval: retryInitial,
		multiplier:      retryMultiplier,
		maxInterval:     max,
		maxBuffered:     size,
		maxBatch:        batch,
		list:            newBufferList(size, batch),
		p:               p,
		logger: 		 log.New(os.Stdout, "retryBuffer", 0),
	}
	go r.run()
	return r
}

func (r *retryBuffer) getStats() map[string]string {
	stats := make(map[string]string)
	stats["buffering"] = strconv.FormatInt(int64(r.buffering), 10)
	for k, v := range r.list.getStats() {
		stats[k] = v
	}
	return stats
}

func (r *retryBuffer) post(buf []byte, query string, auth string, endpoint string) (*responseData, error) {
	if atomic.LoadInt32(&r.buffering) == 0 {
		resp, err := r.p.post(buf, query, auth, endpoint)

		// TODO: A 5xx caused by the point data could cause the relay to buffer forever
		if err == nil && resp.StatusCode/100 != 5 {
			return resp, err
		}

		atomic.StoreInt32(&r.buffering, 1)
	}

	// already buffering or failed request
	_, err := r.list.add(buf, query, auth, endpoint)
	r.logger.Printf("Add data to buffer, the size is %d", r.list.size)

	// batch.wg.Wait()
	// We do not wait for the WaitGroup because we don't want
	// to leave the connection open
	// The client will receive a 202 which closes the connection and
	// invites him to send further requests
	return &responseData{StatusCode: http.StatusAccepted}, err
}

func (r *retryBuffer) run() {
	buf := bytes.NewBuffer(make([]byte, 0, r.maxBatch))
	for {
		r.logger.Printf("Next batch, current size is %d", r.list.size)
		buf.Reset()
		batch := r.list.pop()

		for _, b := range batch.bufs {
			buf.Write(b)
		}

		interval := r.initialInterval
		for {
			if r.flushing == 1 {
				atomic.StoreInt32(&r.buffering, 0)
				batch.wg.Done()

				if r.list.size == 0 {
					atomic.StoreInt32(&r.flushing, 0)
				}

				break
			}

      resp, err := r.p.post(buf.Bytes(), batch.query, batch.auth, batch.endpoint)
      if err == nil && resp.StatusCode/100 != 5 {
				batch.resp = resp
				atomic.StoreInt32(&r.buffering, 0)
				batch.wg.Done()
				break
			}

			if interval != r.maxInterval {
				interval *= r.multiplier
				if interval > r.maxInterval {
					interval = r.maxInterval
				}
			}

			time.Sleep(interval)
		}
	}
}

type batch struct {
	query    string
	auth     string
	bufs     [][]byte
	size     int
	full     bool
	endpoint string

	wg   sync.WaitGroup
	resp *responseData

	next *batch
}

func newBatch(buf []byte, query string, auth string, endpoint string) *batch {
	b := new(batch)
	b.bufs = [][]byte{buf}
	b.size = len(buf)
	b.query = query
	b.auth = auth
	b.endpoint = endpoint
	b.wg.Add(1)
	return b
}

type bufferList struct {
	cond     *sync.Cond
	head     *batch
	size     int
	maxSize  int
	maxBatch int
}

func newBufferList(maxSize, maxBatch int) *bufferList {
	return &bufferList{
		cond:     sync.NewCond(new(sync.Mutex)),
		maxSize:  maxSize,
		maxBatch: maxBatch,
	}
}

func (l *bufferList) getStats() map[string]string {
	stats := make(map[string]string)
	stats["size"] = strconv.FormatInt(int64(l.size), 10)
	stats["maxSize"] = strconv.FormatInt(int64(l.maxSize), 10)
	return stats
}

// Empty the buffer to drop any buffered query
// This allows to flush 'impossible' queries which loop infinitely
// without having to restart the whole relay
func (r *retryBuffer) empty() {
	atomic.StoreInt32(&r.flushing, 1)
}

// pop will remove and return the first element of the list, blocking if necessary
func (l *bufferList) pop() *batch {
	l.cond.L.Lock()

	for l.size == 0 {
		l.cond.Wait()
	}

	b := l.head
	l.head = l.head.next
	l.size -= b.size

	l.cond.L.Unlock()

	return b
}

func (l *bufferList) add(buffer []byte, query string, auth string, endpoint string) (*batch, error) {
	l.cond.L.Lock()

	var buf = make([]byte, len(buffer));
	copy(buf, buffer)

	if l.size+len(buf) > l.maxSize {
		l.cond.L.Unlock()
		return nil, ErrBufferFull
	}

	l.size += len(buf)
	l.cond.Signal()

	var cur **batch

	// non-nil batches that either don't match the query string, don't match the auth
	// credentials, or would be too large when adding the current set of points
	// (auth must be checked to prevent potential problems in multi-user scenarios)
	for cur = &l.head; *cur != nil; cur = &(*cur).next {
		if (*cur).query != query || (*cur).auth != auth || (*cur).full {
			continue
		}

		if (*cur).size+len(buf) > l.maxBatch {
			// prevent future writes from preceding this write
			(*cur).full = true
			continue
		}

		break
	}

	if *cur == nil {
		// new tail element
		*cur = newBatch(buf, query, auth, endpoint)
	} else {
		// append to current batch
		b := *cur

		b.size += len(buf)
		b.bufs = append(b.bufs, buf)
	}

	l.cond.L.Unlock()
	return *cur, nil
}
