/*
Copyright 2022 The Numaproj Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jetstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/numaproj/numaflow/pkg/isb"
	jsclient "github.com/numaproj/numaflow/pkg/shared/clients/nats"
	"github.com/numaproj/numaflow/pkg/shared/logging"
)

type jetStreamReader struct {
	name                   string
	stream                 string
	subject                string
	conn                   *jsclient.NatsConn
	js                     *jsclient.JetStreamContext
	sub                    *nats.Subscription
	opts                   *readOptions
	inProgressTickDuration time.Duration
	partitionIdx           int32
	log                    *zap.SugaredLogger
}

// NewJetStreamBufferReader is used to provide a new JetStream buffer reader connection
func NewJetStreamBufferReader(ctx context.Context, client jsclient.JetStreamClient, name, stream, subject string, partitionIdx int32, opts ...ReadOption) (isb.BufferReader, error) {
	log := logging.FromContext(ctx).With("bufferReader", name).With("stream", stream).With("subject", subject)
	o := defaultReadOptions()
	for _, opt := range opts {
		if opt != nil {
			if err := opt(o); err != nil {
				return nil, err
			}
		}
	}
	result := &jetStreamReader{
		name:         name,
		stream:       stream,
		subject:      subject,
		partitionIdx: partitionIdx,
		opts:         o,
		log:          log,
	}

	connectAndSubscribe := func() (*jsclient.NatsConn, *jsclient.JetStreamContext, *nats.Subscription, error) {
		conn, err := client.Connect(ctx, jsclient.ReconnectHandler(func(c *jsclient.NatsConn) {
			if result.js == nil {
				log.Error("JetStreamContext is nil")
				return
			}
			var e error
			_ = wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
				Steps:    5,
				Duration: 1 * time.Second,
				Factor:   1.0,
				Jitter:   0.1,
			}, func() (bool, error) {
				var s *nats.Subscription
				if s, e = result.js.PullSubscribe(subject, stream, nats.Bind(stream, stream)); e != nil {
					log.Errorw("Failed to re-subscribe to the stream after reconnection, will retry if the limit is not reached", zap.Error(e))
					return false, nil
				} else {
					result.sub = s
					log.Info("Re-subscribed to the stream successfully")
					return true, nil
				}
			})
			if e != nil {
				// Let it panic to start over
				log.Fatalw("Failed to re-subscribe after retries", zap.Error(e))
			}
		}), jsclient.DisconnectErrHandler(func(nc *jsclient.NatsConn, err error) {
			log.Errorw("Nats JetStream connection lost", zap.Error(err))
		}))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to get nats connection, %w", err)
		}
		js, err := conn.JetStream()
		if err != nil {
			conn.Close()
			return nil, nil, nil, fmt.Errorf("failed to get jetstream context, %w", err)
		}
		sub, err := js.PullSubscribe(subject, stream, nats.Bind(stream, stream))
		if err != nil {
			conn.Close()
			return nil, nil, nil, fmt.Errorf("failed to subscribe jet stream subject %q, %w", subject, err)
		}
		return conn, js, sub, nil
	}

	conn, js, sub, err := connectAndSubscribe()
	if err != nil {
		return nil, err
	}

	consumer, err := js.ConsumerInfo(stream, stream)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to get consumer info, %w", err)
	}
	// If ackWait is 3s, ticks every 2s.
	inProgessTickSeconds := int64(consumer.Config.AckWait.Seconds() * 2 / 3)
	if inProgessTickSeconds < 1 {
		inProgessTickSeconds = 1
	}

	result.conn = conn
	result.js = js
	result.sub = sub
	result.inProgressTickDuration = time.Duration(inProgessTickSeconds * int64(time.Second))
	return result, nil
}

func (jr *jetStreamReader) GetName() string {
	return jr.name
}

func (jr *jetStreamReader) GetPartitionIdx() int32 {
	return jr.partitionIdx
}

func (jr *jetStreamReader) Close() error {
	if jr.sub != nil {
		if err := jr.sub.Unsubscribe(); err != nil {
			jr.log.Errorw("Failed to unsubscribe", zap.Error(err))
		}
	}
	if jr.conn != nil && !jr.conn.IsClosed() {
		jr.conn.Close()
	}
	return nil
}

func (jr *jetStreamReader) Pending(_ context.Context) (int64, error) {
	c, err := jr.js.ConsumerInfo(jr.stream, jr.stream)
	if err != nil {
		return isb.PendingNotAvailable, fmt.Errorf("failed to get consumer info, %w", err)
	}
	return int64(c.NumPending) + int64(c.NumAckPending), nil
}

func (jr *jetStreamReader) Read(_ context.Context, count int64) ([]*isb.ReadMessage, error) {
	var err error
	var result []*isb.ReadMessage
	msgs, err := jr.sub.Fetch(int(count), nats.MaxWait(jr.opts.readTimeOut))
	if err != nil && !errors.Is(err, nats.ErrTimeout) {
		isbReadErrors.With(map[string]string{"buffer": jr.GetName()}).Inc()
		return nil, fmt.Errorf("failed to fetch messages from jet stream subject %q, %w", jr.subject, err)
	}
	for _, msg := range msgs {
		var m = new(isb.Message)
		// err should be nil as we have our own marshaller/unmarshaller
		err = m.UnmarshalBinary(msg.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal the message into isb.Message, %w", err)
		}
		msgMetadata, err := msg.Metadata()
		if err != nil {
			return nil, fmt.Errorf("failed to get jetstream message metadata, %w", err)
		}
		rm := &isb.ReadMessage{
			ReadOffset: newOffset(msg, jr.inProgressTickDuration, jr.log),
			Message:    *m,
			Metadata: isb.MessageMetadata{
				NumDelivered: msgMetadata.NumDelivered,
			},
		}
		result = append(result, rm)
	}
	return result, nil
}

func (jr *jetStreamReader) Ack(_ context.Context, offsets []isb.Offset) []error {
	errs := make([]error, len(offsets))
	done := make(chan struct{})
	wg := &sync.WaitGroup{}
	for idx, o := range offsets {
		wg.Add(1)
		go func(index int, o isb.Offset) {
			defer wg.Done()
			if err := o.AckIt(); err != nil {
				jr.log.Errorw("Failed to ack message", zap.Error(err))
				// If the error is related to nats/jetstream, we skip it because it might end up with infinite ack retries.
				// Skipping those errors to let the whole read/write/ack for loop to restart from reading, to pick up those
				// redelivered messages.
				if !strings.HasPrefix(err.Error(), "nats:") {
					errs[index] = err
				}
			}
		}(idx, o)
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	<-done
	return errs
}

func (jr *jetStreamReader) NoAck(ctx context.Context, offsets []isb.Offset) {
	wg := &sync.WaitGroup{}
	for _, o := range offsets {
		wg.Add(1)
		go func(o isb.Offset) {
			defer wg.Done()
			// Ignore the returned error as the worst case the message will
			// take longer to be redelivered.
			if err := o.NoAck(); err != nil {
				jr.log.Errorw("Failed to nak JetStream msg", zap.Error(err))
			}
		}(o)
	}
	wg.Wait()
}

// offset implements ID interface for JetStream.
type offset struct {
	seq        uint64
	msg        *nats.Msg
	cancelFunc context.CancelFunc
}

func newOffset(msg *nats.Msg, tickDuration time.Duration, log *zap.SugaredLogger) *offset {
	metadata, _ := msg.Metadata()
	o := &offset{
		seq: metadata.Sequence.Stream,
		msg: msg,
	}
	// If tickDuration is 1s, which means ackWait is 1s or 2s, it does not make much sense to do it, instead, increasing ackWait is recommended.
	if tickDuration.Seconds() > 1 {
		ctx, cancel := context.WithCancel(context.Background())
		go o.workInProgress(ctx, msg, tickDuration, log)
		o.cancelFunc = cancel
	}
	return o
}

func (o *offset) workInProgress(ctx context.Context, msg *nats.Msg, tickDuration time.Duration, log *zap.SugaredLogger) {
	ticker := time.NewTicker(tickDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			log.Debugw("Mark message processing in generic", zap.Any("seq", o.seq))
			if err := msg.InProgress(); err != nil && !errors.Is(err, nats.ErrMsgAlreadyAckd) && !errors.Is(err, nats.ErrMsgNotFound) {
				log.Errorw("Failed to set JetStream msg in generic", zap.Error(err))
			}
		case <-ctx.Done():
			return
		}
	}
}

func (o *offset) String() string {
	return fmt.Sprint(o.seq)
}

func (o *offset) AckIt() error {
	if o.cancelFunc != nil {
		o.cancelFunc()
	}
	if err := o.msg.AckSync(); err != nil && !errors.Is(err, nats.ErrMsgAlreadyAckd) && !errors.Is(err, nats.ErrMsgNotFound) {
		return err
	}
	return nil
}

func (o *offset) NoAck() error {
	if o.cancelFunc != nil {
		o.cancelFunc()
	}
	return o.msg.Nak()
}

func (o *offset) Sequence() (int64, error) {
	return int64(o.seq), nil
}
