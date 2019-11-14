package buffer

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	cmn "github.com/thetatoken/theta/p2pl/common"
	"github.com/thetatoken/theta/p2pl/transport/buffer/flowrate"
)

//
// SendBuffer
//

type RecvBuffer struct {
	workspace []byte

	queue     chan []byte
	queueSize int32

	rawStream   cmn.ReadWriteCloser
	recvMonitor *flowrate.Monitor

	config RecvBufferConfig
	seqID  int32

	onError cmn.ErrorHandler

	// Life cycle
	wg      *sync.WaitGroup
	quit    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
}

type RecvBufferConfig struct {
	workspaceCapacity int
	RecvRate          int64
	queueCapacity     int
	timeOut           time.Duration
}

// NewRecvBuffer creates a RecvBuffer instance for the given config
func NewRecvBuffer(config RecvBufferConfig, rawStream cmn.ReadWriteCloser) RecvBuffer {
	return RecvBuffer{
		workspace:   make([]byte, 0, config.workspaceCapacity),
		queue:       make(chan []byte, config.queueCapacity),
		rawStream:   rawStream,
		recvMonitor: flowrate.New(0, 0),
		config:      config,
		wg:          &sync.WaitGroup{},
	}
}

// GetDefaultRecvBufferConfig returns the default config for the RecvBuffer
func GetDefaultRecvBufferConfig() RecvBufferConfig {
	return RecvBufferConfig{
		workspaceCapacity: cmn.MaxChunkSize,
		RecvRate:          cmn.MaxRecvRate, // 64 Mbps
		queueCapacity:     1,
		timeOut:           10 * time.Second,
	}
}

func (rb *RecvBuffer) Start(ctx context.Context) bool {
	ctx, cancel := context.WithCancel(ctx)
	rb.ctx = ctx
	rb.cancel = cancel

	rb.wg.Add(1)
	go rb.recvRoutine()

	return true
}

// Wait suspends the caller goroutine
func (rb *RecvBuffer) Wait() {
	rb.wg.Wait()
}

// Stop is called when the RecvBuffer stops
func (rb *RecvBuffer) Stop() {
	rb.cancel()
}

// Read blocks until a message can be retrived from the queue
func (rb *RecvBuffer) Read() []byte {
	msg := <-rb.queue
	atomic.AddInt32(&rb.queueSize, 1)
	return msg
}

// GetSize returns the size of the SendBuffer. It is goroutine safe
func (rb *RecvBuffer) GetSize() int {
	return int(atomic.LoadInt32(&rb.queueSize))
}

// TODO: protection for attacks, e.g. send a very large message to peers
func (rb *RecvBuffer) recvRoutine() {
	defer rb.wg.Done()
	defer rb.recover()

	bytes := make([]byte, cmn.MaxChunkSize)
	for {
		select {
		case <-rb.ctx.Done():
			return
		default:
		}

		// Block until recvMonitor allows reading
		rb.recvMonitor.Limit(cmn.MaxChunkSize, atomic.LoadInt64(&rb.config.RecvRate), true)
		numBytesRead, err := rb.rawStream.Read(bytes)

		if err != nil {
			//log.Errorf("RecvBuffer failed to read data: %v", err)
			//break
			continue
		}

		chunkStart := 0
		for chunkStart < numBytesRead {
			payloadSize := int(int32FromBytes(bytes[chunkStart+4 : chunkStart+8]))
			chunkSize := headerSize + payloadSize

			chunk, err := NewChunkFromRawBytes(bytes[chunkStart : chunkStart+chunkSize])
			if err == nil {
				rb.recvMonitor.Update(chunkSize)

				completeMessage, success := rb.aggregateChunk(chunk)
				if success {
					if completeMessage != nil {
						rb.queue <- completeMessage
						atomic.AddInt32(&rb.queueSize, 1)
					}
				}
			} else {
				log.Errorf("RecvBuffer failed to create new chunk from raw bytes: %v", err)
			}

			chunkStart += chunkSize
		}
	}
}

// aggregateChunk aggregates incoming chunks. It returns the message bytes if the message is
// complete (i.e. ends with EOF). It is not goroutine safe
func (rb *RecvBuffer) aggregateChunk(chunk *Chunk) (completeMessage []byte, success bool) {
	// Note: We do NOT need to worry about the order of the chunks.
	//       TCP guarantees that if bytes arrive, they will be in the
	//       order they were sent, as long as the TCP connection stays open.
	//       But we do need to check if there's any missing chunk
	if rb.seqID != chunk.SeqID() {
		log.Warnf("chunk seqID mismatch. expected: %v, actual: %v", rb.seqID, chunk.SeqID())
		return nil, false
	}

	chunkPayload := chunk.Payload()
	log.Debugf("Aggregate chunk: payloadSize = %v, seqID = %v, isEOF = %v", len(chunkPayload), chunk.SeqID(), chunk.IsEOF())

	rb.workspace = append(rb.workspace, chunkPayload...)
	if chunk.IsEOF() {
		msgSize := len(rb.workspace)
		completeMessage := make([]byte, msgSize)
		copy(completeMessage, rb.workspace)

		// clear the slice without re-allocating.
		// http://stackoverflow.com/questions/16971741/how-do-you-clear-a-slice-in-go
		//   suggests this could be a memory leak, but we might as well keep the memory for the buffer until it closes,
		//	at which point the recving slice stops being used and should be garbage collected
		rb.workspace = rb.workspace[:0] // make([]byte, 0, rb.config.workspaceCapacity)
		rb.seqID = 0

		return completeMessage, true
	}

	rb.seqID++
	return nil, true
}

func (rb *RecvBuffer) recover() {
	if r := recover(); r != nil {
		stack := debug.Stack()
		err := fmt.Errorf(string(stack))
		if rb.onError != nil {
			rb.onError(err)
		}
	}
}