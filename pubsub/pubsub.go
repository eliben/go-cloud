// Copyright 2018 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package pubsub provides an easy and portable way to interact with publish/
// subscribe systems.
//
// Subpackages contain distinct implementations of pubsub for various providers,
// including Cloud and on-prem solutions. For example, "gcppubsub" supports
// Google Cloud Pub/Sub. Your application should import one of these
// provider-specific subpackages and use its exported functions to get a
// *Topic and/or *Subscription; do not use the NewTopic/NewSubscription
// functions in this package. For example:
//
//  topic := mempubsub.NewTopic()
//  err := topic.Send(ctx.Background(), &pubsub.Message{Body: []byte("hi"))
//  ...
//
// Then, write your application code using the *Topic/*Subscription types. You
// can easily reconfigure your initialization code to choose a different provider.
// You can develop your application locally using memblob, or deploy it to
// multiple Cloud providers. You may find http://github.com/google/wire useful
// for managing your initialization code.
//
// Alternatively, you can construct a *Topic/*Subscription via a URL and
// OpenTopic/OpenSubscription.
// See https://godoc.org/gocloud.dev#URLs for more information.
//
//
// At-most-once vs. At-least-once Delivery
//
// Some PubSub systems guarantee that messages received by subscribers but not
// acknowledged are delivered again. These at-least-once systems require that
// subscribers call an ack function to indicate that they have fully processed a
// message.
//
// In other PubSub systems, a message will be delivered only once, if it is delivered
// at all. These at-most-once systems do not need an Ack method.
//
// This package accommodates both kinds of systems. If your application uses
// at-least-once providers, it should always call Message.Ack. If your application
// only uses at-most-once providers, it may call Message.Ack, but does not need to.
// The constructor for at-most-once-providers will require you to supply a function
// to be called whenever the application calls Message.Ack. Common implementations
// are: do nothing, on the grounds that you may want to test your at-least-once
// system with an at-most-once provider; or panic, so that a system that assumes
// at-least-once delivery isn't accidentally paired with an at-most-once provider.
// Providers that support both at-most-once and at-least-once semantics will include
// an optional ack function in their Options struct.
//
//
// OpenCensus Integration
//
// OpenCensus supports tracing and metric collection for multiple languages and
// backend providers. See https://opencensus.io.
//
// This API collects OpenCensus traces and metrics for the following methods:
//  - Topic.Send
//  - Topic.Shutdown
//  - Subscription.Receive
//  - Subscription.Shutdown
//  - The internal driver methods SendBatch, SendAcks and ReceiveBatch.
// All trace and metric names begin with the package import path.
// The traces add the method name.
// For example, "gocloud.dev/pubsub/Topic.Send".
// The metrics are "completed_calls", a count of completed method calls by provider,
// method and status (error code); and "latency", a distribution of method latency
// by provider and method.
// For example, "gocloud.dev/pubsub/latency".
//
// To enable trace collection in your application, see "Configure Exporter" at
// https://opencensus.io/quickstart/go/tracing.
// To enable metric collection in your application, see "Exporting stats" at
// https://opencensus.io/quickstart/go/metrics.
package pubsub // import "gocloud.dev/pubsub"

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	gax "github.com/googleapis/gax-go"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/batcher"
	"gocloud.dev/internal/gcerr"
	"gocloud.dev/internal/oc"
	"gocloud.dev/internal/openurl"
	"gocloud.dev/internal/retry"
	"gocloud.dev/pubsub/driver"
)

// Message contains data to be published.
type Message struct {
	// Body contains the content of the message.
	Body []byte

	// Metadata has key/value metadata for the message.
	Metadata map[string]string

	// processingStartTime is the time that this message was returned
	// from Receive, or the zero time if it wasn't.
	processingStartTime time.Time

	// asFunc invokes driver.Message.AsFunc.
	asFunc func(interface{}) bool

	// ack is a closure that queues this message for acknowledgement.
	ack func()
	// mu guards isAcked in case Ack() is called concurrently.
	mu sync.Mutex

	// isAcked tells whether this message has already had its Ack method
	// called.
	isAcked bool
}

// Ack acknowledges the message, telling the server that it does not need to be
// sent again to the associated Subscription. It returns immediately, but the
// actual ack is sent in the background, and is not guaranteed to succeed.
func (m *Message) Ack() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isAcked {
		panic(fmt.Sprintf("Ack() called twice on message: %+v", m))
	}
	m.ack()
	m.isAcked = true
}

// As converts i to provider-specific types.
// See https://godoc.org/gocloud.dev#As for background information, the "As"
// examples in this package for examples, and the provider-specific package
// documentation for the specific types supported for that provider.
// As panics unless it is called on a message obtained from Subscription.Receive.
func (m *Message) As(i interface{}) bool {
	if m.asFunc == nil {
		panic("As called on a Message that was not obtained from Receive")
	}
	return m.asFunc(i)
}

// Topic publishes messages to all its subscribers.
type Topic struct {
	driver  driver.Topic
	batcher driver.Batcher
	tracer  *oc.Tracer
	mu      sync.Mutex
	err     error

	// cancel cancels all SendBatch calls.
	cancel func()
}

type msgErrChan struct {
	msg     *Message
	errChan chan error
}

// Send publishes a message. It only returns after the message has been
// sent, or failed to be sent. Send can be called from multiple goroutines
// at once.
func (t *Topic) Send(ctx context.Context, m *Message) (err error) {
	ctx = t.tracer.Start(ctx, "Topic.Send")
	defer func() { t.tracer.End(ctx, err) }()

	// Check for doneness before we do any work.
	if err := ctx.Err(); err != nil {
		return err // Return context errors unwrapped.
	}
	t.mu.Lock()
	err = t.err
	t.mu.Unlock()
	if err != nil {
		return err // t.err wrapped when set
	}
	for k, v := range m.Metadata {
		if !utf8.ValidString(k) {
			return fmt.Errorf("pubsub.Send: Message.Metadata keys must be valid UTF-8 strings: %q", k)
		}
		if !utf8.ValidString(v) {
			return fmt.Errorf("pubsub.Send: Message.Metadata values must be valid UTF-8 strings: %q", v)
		}
	}
	return t.batcher.Add(ctx, m)
}

// Shutdown flushes pending message sends and disconnects the Topic.
// It only returns after all pending messages have been sent.
func (t *Topic) Shutdown(ctx context.Context) (err error) {
	ctx = t.tracer.Start(ctx, "Topic.Shutdown")
	defer func() { t.tracer.End(ctx, err) }()

	t.mu.Lock()
	t.err = gcerr.Newf(gcerr.FailedPrecondition, nil, "pubsub: Topic closed")
	t.mu.Unlock()
	c := make(chan struct{})
	go func() {
		defer close(c)
		t.batcher.Shutdown()
	}()
	select {
	case <-ctx.Done():
	case <-c:
	}
	t.cancel()
	return ctx.Err()
}

// As converts i to provider-specific types.
// See https://godoc.org/gocloud.dev#As for background information, the "As"
// examples in this package for examples, and the provider-specific package
// documentation for the specific types supported for that provider.
func (t *Topic) As(i interface{}) bool {
	return t.driver.As(i)
}

// ErrorAs converts err to provider-specific types.
// ErrorAs panics if i is nil or not a pointer.
// ErrorAs returns false if err == nil.
// See https://godoc.org/gocloud.dev#As for background information.
func (t *Topic) ErrorAs(err error, i interface{}) bool {
	return gcerr.ErrorAs(err, i, t.driver.ErrorAs)
}

// NewTopic is for use by provider implementations.
var NewTopic = newTopic

// defaultBatcher creates a batcher for topics, for use with NewTopic.
func defaultBatcher(ctx context.Context, t *Topic, dt driver.Topic) driver.Batcher {
	const maxHandlers = 1
	handler := func(items interface{}) error {
		ms := items.([]*Message)
		var dms []*driver.Message
		for _, m := range ms {
			dm := &driver.Message{
				Body:     m.Body,
				Metadata: m.Metadata,
			}
			dms = append(dms, dm)
		}
		err := retry.Call(ctx, gax.Backoff{}, t.driver.IsRetryable, func() (err error) {
			ctx2 := t.tracer.Start(ctx, "driver.Topic.SendBatch")
			defer func() { t.tracer.End(ctx2, err) }()
			return dt.SendBatch(ctx2, dms)
		})
		if err != nil {
			return wrapError(dt, err)
		}
		return nil
	}
	return batcher.New(reflect.TypeOf(&Message{}), maxHandlers, handler)
}

// newTopic makes a pubsub.Topic from a driver.Topic.
func newTopic(d driver.Topic, newBatcher func(context.Context, *Topic, driver.Topic) driver.Batcher) *Topic {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Topic{
		driver: d,
		tracer: newTracer(d),
		cancel: cancel,
	}
	if newBatcher == nil {
		newBatcher = defaultBatcher
	}
	t.batcher = newBatcher(ctx, t, d)
	return t
}

const pkgName = "gocloud.dev/pubsub"

var (
	latencyMeasure = oc.LatencyMeasure(pkgName)

	// OpenCensusViews are predefined views for OpenCensus metrics.
	// The views include counts and latency distributions for API method calls.
	// See the example at https://godoc.org/go.opencensus.io/stats/view for usage.
	OpenCensusViews = oc.Views(pkgName, latencyMeasure)
)

func newTracer(driver interface{}) *oc.Tracer {
	return &oc.Tracer{
		Package:        pkgName,
		Provider:       oc.ProviderName(driver),
		LatencyMeasure: latencyMeasure,
	}
}

// Subscription receives published messages.
type Subscription struct {
	driver driver.Subscription
	tracer *oc.Tracer
	// ackBatcher makes batches of acks and sends them to the server.
	ackBatcher driver.Batcher
	ackFunc    func() // if non-nil, used for Ack
	cancel     func() // for canceling all SendAcks calls

	mu             sync.Mutex    // protects everything below
	q              []*Message    // local queue of messages downloaded from server
	err            error         // permanent error
	waitc          chan struct{} // for goroutines waiting on ReceiveBatch
	avgProcessTime float64       // moving average of the seconds to process a message
}

const (
	// The desired duration of a subscription's queue of messages (the messages pulled
	// and waiting in memory to be doled out to Receive callers). This is how long
	// it would take to drain the queue at the current processing rate.
	// The relationship to queue length (number of messages) is
	//
	//      lengthInMessages = desiredQueueDuration / averageProcessTimePerMessage
	//
	// In other words, if it takes 100ms to process a message on average, and we want
	// 2s worth of queued messages, then we need 2/.1 = 20 messages in the queue.
	//
	// If desiredQueueDuration is too small, then there won't be a large enough buffer
	// of messages to handle fluctuations in processing time, and the queue is likely
	// to become empty, reducing throughput. If desiredQueueDuration is too large, then
	// messages will wait in memory for a long time, possibly timing out (that is,
	// their ack deadline will be exceeded). Those messages could have been handled
	// by another process receiving from the same subscription.
	desiredQueueDuration = 2 * time.Second

	// The factor by which old points decay when a new point is added to the moving
	// average. The larger this number, the more weight will be given to the newest
	// point in preference to older ones.
	decay = 0.05
)

// Add message processing time d to the weighted moving average.
func (s *Subscription) addProcessingTime(d time.Duration) {
	// Ensure d is non-zero; on some platforms, the granularity of time.Time
	// isn't small enough to capture very fast message processing times.
	if d == 0 {
		d = 1 * time.Nanosecond
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.avgProcessTime == 0 {
		s.avgProcessTime = d.Seconds()
	} else {
		s.avgProcessTime = s.avgProcessTime*(1-decay) + d.Seconds()*decay
	}
}

// Receive receives and returns the next message from the Subscription's queue,
// blocking and polling if none are available. This method can be called
// concurrently from multiple goroutines. The Ack() method of the returned
// Message has to be called once the message has been processed, to prevent it
// from being received again.
func (s *Subscription) Receive(ctx context.Context) (_ *Message, err error) {
	ctx = s.tracer.Start(ctx, "Subscription.Receive")
	defer func() { s.tracer.End(ctx, err) }()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		// The lock is always held here, at the top of the loop.
		if s.err != nil {
			// The Subscription is in a permanent error state. Return the error.
			return nil, s.err // s.err wrapped when set
		}
		if len(s.q) > 0 {
			// At least one message is available. Return it.
			m := s.q[0]
			s.q = s.q[1:]
			m.processingStartTime = time.Now()
			// TODO(jba): pre-fetch more messages if the queue gets too small.
			return m, nil
		}
		if s.waitc != nil {
			// A call to ReceiveBatch is in flight. Wait for it.
			// TODO(jba): support multiple calls in flight simultaneously.
			waitc := s.waitc
			s.mu.Unlock()
			select {
			case <-waitc:
				s.mu.Lock()
				continue
			case <-ctx.Done():
				s.mu.Lock()
				return nil, ctx.Err()
			}
		}
		// No messages are available and there are no calls to ReceiveBatch in flight.
		// Make a call.
		//
		// Ask for a number of messages that will give us the desired queue length.
		// Unless we don't have information about process time (at the beginning), in
		// which case just get one message.
		nMessages := 1
		if s.avgProcessTime > 0 {
			// Using Ceil guarantees at least one message.
			n := math.Ceil(desiredQueueDuration.Seconds() / s.avgProcessTime)
			// Cap nMessages at some non-ridiculous value.
			// Slight hack: we should be using a larger cap, like MaxInt32. But
			// that messes up replay: since the tests take very little time to ack,
			// n is very large, and since our averaging process is time-sensitive,
			// values can differ slightly from run to run. The current cap happens
			// to work, but we should come up with a more robust solution.
			// (Currently it doesn't matter for performance, because gcppubsub
			// caps maxMessages to 1000 anyway.)
			nMessages = int(math.Min(n, 1000))
		}
		s.waitc = make(chan struct{})
		s.mu.Unlock()
		// Even though the mutex is unlocked, only one goroutine can be here.
		// The only way here is if s.waitc was nil. This goroutine just set
		// s.waitc to non-nil while holding the lock.
		msgs, err := s.getNextBatch(ctx, nMessages)
		s.mu.Lock()
		close(s.waitc)
		s.waitc = nil
		if err != nil {
			// This goroutine's call failed, perhaps because its context was done.
			// Some waiting goroutine will wake up when s.waitc is closed,
			// go to the top of the loop, and (since s.q is empty and s.waitc is
			// now nil) will try the RPC for itself.
			return nil, err
		}
		s.q = append(s.q, msgs...)
	}
}

// getNextBatch gets the next batch of messages from the server and returns it.
func (s *Subscription) getNextBatch(ctx context.Context, nMessages int) ([]*Message, error) {
	var msgs []*driver.Message
	err := retry.Call(ctx, gax.Backoff{}, s.driver.IsRetryable, func() error {
		var err error
		ctx2 := s.tracer.Start(ctx, "driver.Subscription.ReceiveBatch")
		defer func() { s.tracer.End(ctx2, err) }()
		msgs, err = s.driver.ReceiveBatch(ctx2, nMessages)
		return err
	})
	if err != nil {
		return nil, wrapError(s.driver, err)
	}
	var q []*Message
	for _, m := range msgs {
		id := m.AckID
		m2 := &Message{
			Body:     m.Body,
			Metadata: m.Metadata,
			asFunc:   m.AsFunc,
		}
		if s.ackFunc == nil {
			m2.ack = func() {
				// Note: This call locks s.mu, and m2.mu is locked here as well. Deadlock
				// will result if Message.Ack is ever called with s.mu held. That
				// currently cannot happen, but we should be careful if/when implementing
				// features like auto-ack.
				s.addProcessingTime(time.Since(m2.processingStartTime))

				// Ignore the error channel. Errors are dealt with
				// in the ackBatcher handler.
				_ = s.ackBatcher.AddNoWait(id)
			}
		} else {
			m2.ack = func() {
				s.addProcessingTime(time.Since(m2.processingStartTime)) // see note above
				s.ackFunc()
			}
		}
		q = append(q, m2)
	}
	return q, nil
}

// Shutdown flushes pending ack sends and disconnects the Subscription.
func (s *Subscription) Shutdown(ctx context.Context) (err error) {
	ctx = s.tracer.Start(ctx, "Subscription.Shutdown")
	defer func() { s.tracer.End(ctx, err) }()

	s.mu.Lock()
	s.err = gcerr.Newf(gcerr.FailedPrecondition, nil, "pubsub: Subscription closed")
	s.mu.Unlock()
	c := make(chan struct{})
	go func() {
		defer close(c)
		s.ackBatcher.Shutdown()
	}()
	select {
	case <-ctx.Done():
	case <-c:
	}
	s.cancel()
	return ctx.Err()
}

// As converts i to provider-specific types.
// See https://godoc.org/gocloud.dev#As for background information, the "As"
// examples in this package for examples, and the provider-specific package
// documentation for the specific types supported for that provider.
func (s *Subscription) As(i interface{}) bool {
	return s.driver.As(i)
}

// ErrorAs converts err to provider-specific types.
// ErrorAs panics if i is nil or not a pointer.
// ErrorAs returns false if err == nil.
// See Topic.As for more details.
func (s *Subscription) ErrorAs(err error, i interface{}) bool {
	return gcerr.ErrorAs(err, i, s.driver.ErrorAs)
}

// NewSubscription is for use by provider implementations.
var NewSubscription = newSubscription

// newSubscription creates a Subscription from a driver.Subscription
// and a function to make a batcher that sends batches of acks to the provider.
// If newAckBatcher is nil, a default batcher implementation will be used.
func newSubscription(ds driver.Subscription, newAckBatcher func(context.Context, *Subscription, driver.Subscription) driver.Batcher) *Subscription {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Subscription{
		driver: ds,
		tracer: newTracer(ds),
		cancel: cancel,
	}
	if newAckBatcher == nil {
		newAckBatcher = defaultAckBatcher
	}
	s.ackBatcher = newAckBatcher(ctx, s, ds)
	s.ackFunc = ds.AckFunc()
	return s
}

// defaultAckBatcher creates a batcher for acknowledgements, for use with
// NewSubscription.
func defaultAckBatcher(ctx context.Context, s *Subscription, ds driver.Subscription) driver.Batcher {
	const maxHandlers = 1
	handler := func(items interface{}) error {
		ids := items.([]driver.AckID)
		err := retry.Call(ctx, gax.Backoff{}, s.driver.IsRetryable, func() (err error) {
			ctx2 := s.tracer.Start(ctx, "driver.Subscription.SendAcks")
			defer func() { s.tracer.End(ctx2, err) }()
			return ds.SendAcks(ctx2, ids)
		})
		// Remember a non-retryable error from SendAcks. It will be returned on the
		// next call to Receive.
		if err != nil {
			err = wrapError(s.driver, err)
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
		}
		return err
	}
	return batcher.New(reflect.TypeOf([]driver.AckID{}).Elem(), maxHandlers, handler)
}

type errorCoder interface {
	ErrorCode(error) gcerrors.ErrorCode
}

func wrapError(ec errorCoder, err error) error {
	if gcerr.DoNotWrap(err) {
		return err
	}
	return gcerr.New(ec.ErrorCode(err), err, 2, "pubsub")
}

// TopicURLOpener represents types than can open Topics based on a URL.
// The opener must not modify the URL argument. OpenTopicURL must be safe to
// call from multiple goroutines.
//
// This interface is generally implemented by types in driver packages.
type TopicURLOpener interface {
	OpenTopicURL(ctx context.Context, u *url.URL) (*Topic, error)
}

// SubscriptionURLOpener represents types than can open Subscriptions based on a URL.
// The opener must not modify the URL argument. OpenSubscriptionURL must be safe to
// call from multiple goroutines.
//
// This interface is generally implemented by types in driver packages.
type SubscriptionURLOpener interface {
	OpenSubscriptionURL(ctx context.Context, u *url.URL) (*Subscription, error)
}

// URLMux is a URL opener multiplexer. It matches the scheme of the URLs
// against a set of registered schemes and calls the opener that matches the
// URL's scheme.
// See https://godoc.org/gocloud.dev#URLs for more information.
//
// The zero value is a multiplexer with no registered schemes.
type URLMux struct {
	subscriptionSchemes openurl.SchemeMap
	topicSchemes        openurl.SchemeMap
}

// RegisterTopic registers the opener with the given scheme. If an opener
// already exists for the scheme, RegisterTopic panics.
func (mux *URLMux) RegisterTopic(scheme string, opener TopicURLOpener) {
	mux.topicSchemes.Register("pubsub", "Topic", scheme, opener)
}

// RegisterSubscription registers the opener with the given scheme. If an opener
// already exists for the scheme, RegisterSubscription panics.
func (mux *URLMux) RegisterSubscription(scheme string, opener SubscriptionURLOpener) {
	mux.subscriptionSchemes.Register("pubsub", "Subscription", scheme, opener)
}

// OpenTopic calls OpenTopicURL with the URL parsed from urlstr.
// OpenTopic is safe to call from multiple goroutines.
func (mux *URLMux) OpenTopic(ctx context.Context, urlstr string) (*Topic, error) {
	opener, u, err := mux.topicSchemes.FromString("Topic", urlstr)
	if err != nil {
		return nil, err
	}
	return opener.(TopicURLOpener).OpenTopicURL(ctx, u)
}

// OpenSubscription calls OpenSubscriptionURL with the URL parsed from urlstr.
// OpenSubscription is safe to call from multiple goroutines.
func (mux *URLMux) OpenSubscription(ctx context.Context, urlstr string) (*Subscription, error) {
	opener, u, err := mux.subscriptionSchemes.FromString("Subscription", urlstr)
	if err != nil {
		return nil, err
	}
	return opener.(SubscriptionURLOpener).OpenSubscriptionURL(ctx, u)
}

// OpenTopicURL dispatches the URL to the opener that is registered with the
// URL's scheme. OpenTopicURL is safe to call from multiple goroutines.
func (mux *URLMux) OpenTopicURL(ctx context.Context, u *url.URL) (*Topic, error) {
	opener, err := mux.topicSchemes.FromURL("Topic", u)
	if err != nil {
		return nil, err
	}
	return opener.(TopicURLOpener).OpenTopicURL(ctx, u)
}

// OpenSubscriptionURL dispatches the URL to the opener that is registered with the
// URL's scheme. OpenSubscriptionURL is safe to call from multiple goroutines.
func (mux *URLMux) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*Subscription, error) {
	opener, err := mux.subscriptionSchemes.FromURL("Subscription", u)
	if err != nil {
		return nil, err
	}
	return opener.(SubscriptionURLOpener).OpenSubscriptionURL(ctx, u)
}

var defaultURLMux = &URLMux{}

// DefaultURLMux returns the URLMux used by OpenTopic and OpenSubscription.
//
// Driver packages can use this to register their TopicURLOpener and/or
// SubscriptionURLOpener on the mux.
func DefaultURLMux() *URLMux {
	return defaultURLMux
}

// OpenTopic opens the Topic identified by the URL given.
// See the URLOpener documentation in provider-specific subpackages for
// details on supported URL formats, and https://godoc.org/gocloud.dev#URLs
// for more information.
func OpenTopic(ctx context.Context, urlstr string) (*Topic, error) {
	return defaultURLMux.OpenTopic(ctx, urlstr)
}

// OpenSubscription opens the Subscription identified by the URL given.
// See the URLOpener documentation in provider-specific subpackages for
// details on supported URL formats, and https://godoc.org/gocloud.dev#URLs
// for more information.
func OpenSubscription(ctx context.Context, urlstr string) (*Subscription, error) {
	return defaultURLMux.OpenSubscription(ctx, urlstr)
}
