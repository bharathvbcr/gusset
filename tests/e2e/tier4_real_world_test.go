package e2e_test

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestTier4_MultiWorker_ETLPipeline simulates an end-to-end data ingestion and batch transformation pipeline
// where multiple worker goroutines consume batches, transform data in Rust, and aggregate results.
func TestTier4_MultiWorker_ETLPipeline(t *testing.T) {
	const poolSize = 8
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type rawRecord struct {
		id   uint32
		data []byte
	}

	const totalRecords = 500
	const batchSize = 25
	recordsCh := make(chan []rawRecord, totalRecords/batchSize)

	// Produce batches
	for b := 0; b < totalRecords/batchSize; b++ {
		batch := make([]rawRecord, batchSize)
		for i := 0; i < batchSize; i++ {
			recID := uint32(b*batchSize + i)
			data := generateDeterministicBytes(64, byte(recID%251))
			batch[i] = rawRecord{id: recID, data: data}
		}
		recordsCh <- batch
	}
	close(recordsCh)

	var processedRecords atomic.Int64
	var totalChecksum atomic.Uint64
	var expectedChecksum atomic.Uint64

	const numWorkers = 4
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range recordsCh {
				// Pack batch into a 64-byte aligned Rust buffer
				// Total payload size: 1 byte (mode 0) + batchSize * 64 bytes
				payloadSize := 1 + len(batch)*64
				buf, err := h.NewBuffer(payloadSize)
				if err != nil {
					t.Errorf("NewBuffer failed: %v", err)
					return
				}

				rawBytes := buf.Bytes()
				rawBytes[0] = 0 // mode 0 echo
				offset := 1
				for _, rec := range batch {
					copy(rawBytes[offset:offset+64], rec.data)
					for _, b := range rec.data {
						expectedChecksum.Add(uint64(b))
					}
					offset += 64
				}

				resBuf, err := h.CallBuffer(ctx, buf)
				buf.Free()
				if err != nil {
					t.Errorf("CallBuffer failed: %v", err)
					return
				}

				// Verify transformed batch
				resData := resBuf.Bytes()
				if len(resData) != payloadSize {
					t.Errorf("expected result length %d, got %d", payloadSize, len(resData))
				}

				resOffset := 1
				for range batch {
					for _, b := range resData[resOffset : resOffset+64] {
						totalChecksum.Add(uint64(b))
					}
					resOffset += 64
					processedRecords.Add(1)
				}
				resBuf.Free()
			}
		}()
	}
	wg.Wait()

	if processedRecords.Load() != int64(totalRecords) {
		t.Fatalf("expected %d processed records, got %d", totalRecords, processedRecords.Load())
	}
	if totalChecksum.Load() != expectedChecksum.Load() {
		t.Fatalf("checksum mismatch: got %d, want %d", totalChecksum.Load(), expectedChecksum.Load())
	}

	t.Logf("ETL Pipeline succeeded: %d records processed, checksum=%d",
		processedRecords.Load(), totalChecksum.Load())
}

// TestTier4_ImageTensor_BatchProcessing simulates image/tensor batch preprocessing where Go prepares
// high-dimensional float/byte buffers, delegates vector computations to Rust, and validates against
// an independent pure-Go mathematical oracle.
func TestTier4_ImageTensor_BatchProcessing(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Simulate batch of 8 images, each 64x64 pixels with 3 channels (RGB) = 12,288 bytes each.
	// Total batch tensor size = 8 * 12,288 = 98,304 bytes.
	const imagesInBatch = 8
	const imageBytes = 64 * 64 * 3
	const totalTensorBytes = imagesInBatch * imageBytes

	tensorData := make([]byte, totalTensorBytes)
	for i := range tensorData {
		tensorData[i] = byte((i * 17) % 256)
	}

	// 1. Calculate ground truth using pure-Go mathematical oracle
	expectedSumSq := computeVectorSumSquaresOracle(tensorData)

	// 2. Allocate 64-byte aligned Rust buffer for tensor batch
	// Payload format: 1 byte opcode (mode 10) + tensorData
	buf, err := h.NewBuffer(1 + totalTensorBytes)
	requireNoError(t, err, "h.NewBuffer tensor batch")
	defer buf.Free()

	bufData := buf.Bytes()
	if !is64ByteAligned(bufData) {
		t.Fatal("tensor buffer is not 64-byte aligned")
	}
	bufData[0] = 10 // opcode 10: vector sum-and-square
	copy(bufData[1:], tensorData)

	// 3. Execute accelerated computation in Rust
	ticket, err := h.Submit(ctx, buf)
	requireNoError(t, err, "h.Submit tensor batch")

	res, err := h.Wait(ctx, ticket)
	requireNoError(t, err, "h.Wait tensor batch")

	if len(res) != 8 {
		t.Fatalf("expected 8-byte uint64 result, got %d bytes", len(res))
	}

	actualSumSq := binary.LittleEndian.Uint64(res)

	// 4. Validate mathematical bit-exact parity
	if actualSumSq != expectedSumSq {
		t.Fatalf("tensor computation parity failure: Rust=%d, Go Oracle=%d", actualSumSq, expectedSumSq)
	}

	t.Logf("Image Tensor Batch (%d bytes) verified: sum-sq = %d", totalTensorBytes, actualSumSq)
}

// TestTier4_HighFrequency_StreamProcessor simulates a high-rate streaming event processor
// using an asynchronous sliding-window pipeline, asserting zero dropped events and steady memory.
func TestTier4_HighFrequency_StreamProcessor(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const totalEvents = 1000
	const slidingWindowSize = 32

	initialStats := gusset.Stats()

	type inFlight struct {
		ticket uint64
		id     int
	}

	window := make(chan inFlight, slidingWindowSize)
	var processedCount atomic.Int64
	var errCount atomic.Int64

	// Receiver loop
	var recvWg sync.WaitGroup
	recvWg.Add(1)
	go func() {
		defer recvWg.Done()
		for item := range window {
			res, err := h.Wait(ctx, item.ticket)
			if err != nil {
				errCount.Add(1)
				t.Errorf("stream item %d failed: %v", item.id, err)
			} else {
				if len(res) < 5 {
					t.Errorf("stream item %d truncated output", item.id)
				}
				processedCount.Add(1)
			}
		}
	}()

	// Feeder loop
	for i := 0; i < totalEvents; i++ {
		payload := make([]byte, 16)
		payload[0] = 0 // mode 0 echo
		binary.LittleEndian.PutUint32(payload[1:5], uint32(i))

		ticket, err := h.Submit(ctx, payload)
		if err != nil {
			t.Fatalf("stream submit failed at event %d: %v", i, err)
		}
		window <- inFlight{ticket: ticket, id: i}
	}
	close(window)
	recvWg.Wait()

	if processedCount.Load() != int64(totalEvents) {
		t.Fatalf("expected %d processed stream events, got %d (errors: %d)",
			totalEvents, processedCount.Load(), errCount.Load())
	}

	// Verify steady state memory: no unbounded live bytes growth
	finalStats := gusset.Stats()
	if finalStats.LiveBytes != initialStats.LiveBytes {
		t.Errorf("stream processing caused live byte drift: initial=%d, final=%d",
			initialStats.LiveBytes, finalStats.LiveBytes)
	}

	t.Logf("High-frequency stream processing complete: %d events processed smoothly", processedCount.Load())
}

// TestTier4_DistributedTracing_MicroserviceSimulation simulates multi-hop RPC microservices
// with W3C-compliant distributed tracing across concurrent requests.
func TestTier4_DistributedTracing_MicroserviceSimulation(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctxRoot, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const numConcurrentRequests = 16
	var wg sync.WaitGroup

	for req := 0; req < numConcurrentRequests; req++ {
		wg.Add(1)
		go func(requestID int) {
			defer wg.Done()

			// Create unique trace carrier for this request
			carrier := newTestTraceCarrier(byte(requestID * 11))
			reqCtx := context.WithValue(ctxRoot, gusset.SpanContextKey, carrier)

			// Execute call to diagnostic mode 7 (echoes trace and span ID)
			res, err := h.Call(reqCtx, []byte{7})
			if err != nil {
				t.Errorf("request %d: Call failed: %v", requestID, err)
				return
			}

			if len(res) != 24 {
				t.Errorf("request %d: expected 24 bytes, got %d", requestID, len(res))
				return
			}

			gotTrace := res[0:16]
			gotSpan := res[16:24]
			wantTrace := carrier.TraceID()
			wantSpan := carrier.SpanID()

			assertBytesEqual(t, gotTrace, wantTrace[:], "trace ID propagation")
			assertBytesEqual(t, gotSpan, wantSpan[:], "span ID propagation")
		}(req)
	}
	wg.Wait()
}
