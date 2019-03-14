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

// Package azurepubsub provides an implementation of pubsub using Azure Service
// Bus Topic and Subscription.
// See https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-messaging-overview for an overview.
//
// URLs
//
// For pubsub.OpenTopic/Subscription URLs, azurepubsub registers for the scheme
// "azuresb". The Service Bus Connection String defaults to the environment
// variable "SERVICEBUS_CONNECTION_STRING". For details on the format of the
// URL, see URLOpener.
//
// As
//
// azurepubsub exposes the following types for As:
//  - Topic: *servicebus.Topic
//  - Subscription: *servicebus.Subscription
//  - Message: *servicebus.Message
//  - Error: common.Retryable
package azurepubsub // import "gocloud.dev/pubsub/azurepubsub"

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"runtime"
	"sync"
	"time"

	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/useragent"
	"gocloud.dev/pubsub"
	"gocloud.dev/pubsub/driver"

	"pack.ag/amqp"

	common "github.com/Azure/azure-amqp-common-go"
	"github.com/Azure/azure-amqp-common-go/cbs"
	"github.com/Azure/azure-amqp-common-go/rpc"
	"github.com/Azure/azure-amqp-common-go/uuid"
	servicebus "github.com/Azure/azure-service-bus-go"
)

const (
	completedStatus = "completed"
	listenerTimeout = 1 * time.Second
	rpcTries        = 5
	rpcRetryDelay   = 1 * time.Second
)

func init() {
	o := new(defaultConnectionStringOpener)
	pubsub.DefaultURLMux().RegisterTopic(Scheme, o)
	pubsub.DefaultURLMux().RegisterSubscription(Scheme, o)
}

// defaultURLOpener creates an URLOpener with ConnectionString initialized from
// the environment variable SERVICEBUS_CONNECTION_STRING.
type defaultConnectionStringOpener struct {
	init   sync.Once
	opener *URLOpener
	err    error
}

func (o *defaultConnectionStringOpener) defaultOpener() (*URLOpener, error) {
	o.init.Do(func() {
		cs := os.Getenv("SERVICEBUS_CONNECTION_STRING")
		if cs == "" {
			o.err = errors.New("SERVICEBUS_CONNECTION_STRING environment variable not set")
			return
		}
		o.opener = &URLOpener{
			ConnectionString: cs,
		}
	})
	return o.opener, o.err
}

func (o *defaultConnectionStringOpener) OpenTopicURL(ctx context.Context, u *url.URL) (*pubsub.Topic, error) {
	opener, err := o.defaultOpener()
	if err != nil {
		return nil, fmt.Errorf("open topic %v: %v", u, err)
	}
	return opener.OpenTopicURL(ctx, u)
}

func (o *defaultConnectionStringOpener) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*pubsub.Subscription, error) {
	opener, err := o.defaultOpener()
	if err != nil {
		return nil, fmt.Errorf("open subscription %v: %v", u, err)
	}
	return opener.OpenSubscriptionURL(ctx, u)
}

// Scheme is the URL scheme azurepubsub registers its URLOpeners under on pubsub.DefaultMux.
const Scheme = "azuresb"

// URLOpener opens Azure Service Bus URLs like "azuresb://mytopic" for
// topics or "azuresb://mysubscription?topic=mytopic" for subscriptions.
//
//   - The URL's host+path is used as the topic name.
//   - For subscriptions, the subscription name must be provided in the
//     "subscription" query parameter.
type URLOpener struct {
	// ConnectionString is the Service Bus connection string.
	// https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-dotnet-get-started-with-queues
	ConnectionString string

	// Options passed when creating the ServiceBus Topic/Subscription.
	ServiceBusTopicOptions        []servicebus.TopicOption
	ServiceBusSubscriptionOptions []servicebus.SubscriptionOption

	// TopicOptions specifies the options to pass to OpenTopic.
	TopicOptions TopicOptions
	// SubscriptionOptions specifies the options to pass to OpenSubscription.
	SubscriptionOptions SubscriptionOptions
}

// Gets the *servicebus.Namespace using a connection string from the
// env, URLOpener, or query param.
func (o *URLOpener) namespace(kind string, u *url.URL) (*servicebus.Namespace, error) {
	if o.ConnectionString == "" {
		return nil, fmt.Errorf("open %s %v: ConnectionString is required", kind, u)
	}
	ns, err := NewNamespaceFromConnectionString(o.ConnectionString)
	if err != nil {
		return nil, fmt.Errorf("open %s %v: invalid connection string %q: %v", kind, u, o.ConnectionString, err)
	}
	return ns, nil
}

// OpenTopicURL opens a pubsub.Topic based on u.
func (o *URLOpener) OpenTopicURL(ctx context.Context, u *url.URL) (*pubsub.Topic, error) {
	ns, err := o.namespace("topic", u)
	if err != nil {
		return nil, err
	}
	for param := range u.Query() {
		return nil, fmt.Errorf("open topic %q: invalid query parameter %q", u, param)
	}
	topicName := path.Join(u.Host, u.Path)
	t, err := NewTopic(ns, topicName, o.ServiceBusTopicOptions)
	if err != nil {
		return nil, fmt.Errorf("open topic %v: couldn't open topic %q: %v", u, topicName, err)
	}
	return OpenTopic(ctx, t, &o.TopicOptions), nil
}

// OpenSubscriptionURL opens a pubsub.Subscription based on u.
func (o *URLOpener) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*pubsub.Subscription, error) {
	ns, err := o.namespace("subscription", u)
	if err != nil {
		return nil, err
	}
	topicName := path.Join(u.Host, u.Path)
	t, err := NewTopic(ns, topicName, o.ServiceBusTopicOptions)
	if err != nil {
		return nil, fmt.Errorf("open subscription %v: couldn't open topic %q: %v", u, topicName, err)
	}
	q := u.Query()
	subName := q.Get("subscription")
	q.Del("subscription")
	if subName == "" {
		return nil, fmt.Errorf("open subscription %q: missing required query parameter subscription", u)
	}
	for param := range q {
		return nil, fmt.Errorf("open subscription %q: invalid query parameter %q", u, param)
	}
	sub, err := NewSubscription(t, subName, o.ServiceBusSubscriptionOptions)
	if err != nil {
		return nil, fmt.Errorf("open subscription %v: couldn't open subscription %q: %v", u, subName, err)
	}
	return OpenSubscription(ctx, ns, t, sub, &o.SubscriptionOptions), nil
}

type topic struct {
	sbTopic *servicebus.Topic
}

// TopicOptions provides configuration options for an Azure SB Topic.
type TopicOptions struct{}

// NewNamespaceFromConnectionString returns a *servicebus.Namespace from a Service Bus connection string.
// https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-dotnet-get-started-with-queues
func NewNamespaceFromConnectionString(connectionString string) (*servicebus.Namespace, error) {
	nsOptions := servicebus.NamespaceWithConnectionString(connectionString)
	return servicebus.NewNamespace(nsOptions)
}

// NewTopic returns a *servicebus.Topic associated with a Service Bus Namespace.
func NewTopic(ns *servicebus.Namespace, topicName string, opts []servicebus.TopicOption) (*servicebus.Topic, error) {
	return ns.NewTopic(topicName, opts...)
}

// NewSubscription returns a *servicebus.Subscription associated with a Service Bus Topic.
func NewSubscription(parentTopic *servicebus.Topic, subscriptionName string, opts []servicebus.SubscriptionOption) (*servicebus.Subscription, error) {
	return parentTopic.NewSubscription(subscriptionName, opts...)
}

// OpenTopic initializes a pubsub Topic on a given Service Bus Topic.
func OpenTopic(ctx context.Context, sbTopic *servicebus.Topic, opts *TopicOptions) *pubsub.Topic {
	t := openTopic(ctx, sbTopic)
	return pubsub.NewTopic(t, nil)
}

// openTopic returns the driver for OpenTopic. This function exists so the test
// harness can get the driver interface implementation if it needs to.
func openTopic(ctx context.Context, sbTopic *servicebus.Topic) driver.Topic {
	return &topic{
		sbTopic: sbTopic,
	}
}

// SendBatch implements driver.Topic.SendBatch.
func (t *topic) SendBatch(ctx context.Context, dms []*driver.Message) error {
	for _, dm := range dms {
		sbms := servicebus.NewMessage(dm.Body)
		for k, v := range dm.Metadata {
			sbms.Set(k, v)
		}
		if err := t.sbTopic.Send(ctx, sbms); err != nil {
			return err
		}
	}
	return nil
}

func (t *topic) IsRetryable(err error) bool {
	// Let the Service Bus SDK recover from any transient connectivity issue.
	return false
}

func (t *topic) As(i interface{}) bool {
	p, ok := i.(**servicebus.Topic)
	if !ok {
		return false
	}
	*p = t.sbTopic
	return true
}

// ErrorAs implements driver.Topic.ErrorAs
func (*topic) ErrorAs(err error, i interface{}) bool {
	return errorAs(err, i)
}

func errorAs(err error, i interface{}) bool {
	switch v := err.(type) {
	case *amqp.Error:
		if p, ok := i.(**amqp.Error); ok {
			*p = v
			return true
		}
	case common.Retryable:
		if p, ok := i.(*common.Retryable); ok {
			*p = v
			return true
		}
	}
	return false
}

func (*topic) ErrorCode(err error) gcerrors.ErrorCode {
	return errorCode(err)
}

type subscription struct {
	sbSub     *servicebus.Subscription
	opts      *SubscriptionOptions
	topicName string                // Used in driver.subscription.SendAcks to validate credentials before issuing the message complete bulk operation.
	sbNs      *servicebus.Namespace // Used in driver.subscription.SendAcks to validate credentials before issuing the message complete bulk operation.
}

// SubscriptionOptions will contain configuration for subscriptions.
type SubscriptionOptions struct {
	ListenerTimeout time.Duration
}

// OpenSubscription initializes a pubsub Subscription on a given Service Bus Subscription and its parent Service Bus Topic.
func OpenSubscription(ctx context.Context, parentNamespace *servicebus.Namespace, parentTopic *servicebus.Topic, sbSubscription *servicebus.Subscription, opts *SubscriptionOptions) *pubsub.Subscription {
	ds := openSubscription(ctx, parentNamespace, parentTopic, sbSubscription, opts)
	return pubsub.NewSubscription(ds, nil)
}

// openSubscription returns a driver.Subscription.
func openSubscription(ctx context.Context, sbNs *servicebus.Namespace, sbTop *servicebus.Topic, sbSub *servicebus.Subscription, opts *SubscriptionOptions) driver.Subscription {
	topicName := ""
	if sbTop != nil {
		topicName = sbTop.Name
	}

	defaultTimeout := listenerTimeout
	if opts != nil && opts.ListenerTimeout > 0 {
		defaultTimeout = opts.ListenerTimeout
	}

	return &subscription{
		sbSub:     sbSub,
		topicName: topicName,
		sbNs:      sbNs,
		opts: &SubscriptionOptions{
			ListenerTimeout: defaultTimeout,
		},
	}
}

// testSBSubscription ensures the subscription exists before listening for incoming messages.
func (s *subscription) testSBSubscription(ctx context.Context) error {
	if s.topicName == "" {
		return errors.New("azurepubsub: driver.Subscription requires a Service Bus Topic")
	}
	if s.sbNs == nil {
		return errors.New("azurepubsub: driver.Subscription requires a Service Bus Namespace")
	}
	if s.sbSub == nil {
		return errors.New("azurepubsub: driver.Subscription requires a Service Bus Subscription")
	}

	sm, err := s.sbNs.NewSubscriptionManager(s.topicName)
	if err != nil {
		return err
	}

	// An empty SubscriptionEntity means no Service Bus Subscription exists for the given name.
	se, _ := sm.Get(ctx, s.sbSub.Name)
	if se == nil {
		return fmt.Errorf("azurepubsub: no such subscription %q", s.sbSub.Name)
	}
	return nil
}

// IsRetryable implements driver.Subscription.IsRetryable.
func (s *subscription) IsRetryable(error) bool {
	// Let the Service Bus SDK recover from any transient connectivity issue.
	return false
}

// As implements driver.Subscription.As.
func (s *subscription) As(i interface{}) bool {
	p, ok := i.(**servicebus.Subscription)
	if !ok {
		return false
	}
	*p = s.sbSub
	return true
}

// ErrorAs implements driver.Subscription.ErrorAs
func (s *subscription) ErrorAs(err error, i interface{}) bool {
	return errorAs(err, i)
}

func (s *subscription) ErrorCode(err error) gcerrors.ErrorCode {
	return errorCode(err)
}

// ReceiveBatch implements driver.Subscription.ReceiveBatch.
func (s *subscription) ReceiveBatch(ctx context.Context, maxMessages int) ([]*driver.Message, error) {
	// Test to ensure existence of the Service Bus Subscription before listening for messages.
	// Listening on a non-existence Service Bus Subscription does not fail. This check is also needed for conformance tests which
	// requires this scenario to fail on ReceiveBatch.
	err := s.testSBSubscription(ctx)
	if err != nil {
		return nil, err
	}

	rctx, cancel := context.WithTimeout(ctx, s.opts.ListenerTimeout)
	defer cancel()
	var messages []*driver.Message
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		s.sbSub.Receive(rctx, servicebus.HandlerFunc(func(innerctx context.Context, sbmsg *servicebus.Message) error {
			metadata := map[string]string{}

			sbmsg.ForeachKey(func(k, v string) error {
				metadata[k] = v
				return nil
			})

			messages = append(messages, &driver.Message{
				Body:     sbmsg.Data,
				Metadata: metadata,
				AckID:    sbmsg.LockToken,
				AsFunc:   messageAsFunc(sbmsg),
			})
			if len(messages) >= maxMessages {
				cancel()
			}
			return nil
		}))

		select {
		case <-rctx.Done():
			wg.Done()
		}
	}()

	wg.Wait()
	return messages, nil
}

func messageAsFunc(sbmsg *servicebus.Message) func(interface{}) bool {
	return func(i interface{}) bool {
		p, ok := i.(**servicebus.Message)
		if !ok {
			return false
		}
		*p = sbmsg
		return true
	}
}

// SendAcks implements driver.Subscription.SendAcks.
// IMPORTANT: This is a workaround to issue 'completed' message dispositions in bulk which is not supported in the Service Bus SDK.
func (s *subscription) SendAcks(ctx context.Context, ids []driver.AckID) error {
	if len(ids) == 0 {
		return nil
	}

	host := fmt.Sprintf("amqps://%s.%s/", s.sbNs.Name, s.sbNs.Environment.ServiceBusEndpointSuffix)
	client, err := amqp.Dial(host,
		amqp.ConnSASLAnonymous(),
		amqp.ConnProperty("product", "Go-Cloud Client"),
		amqp.ConnProperty("version", servicebus.Version),
		amqp.ConnProperty("platform", runtime.GOOS),
		amqp.ConnProperty("framework", runtime.Version()),
		amqp.ConnProperty("user-agent", useragent.AzureUserAgentPrefix("pubsub")),
	)
	if err != nil {
		return err
	}
	defer client.Close()

	entityPath := s.topicName + "/Subscriptions/" + s.sbSub.Name
	audience := host + entityPath
	err = cbs.NegotiateClaim(ctx, audience, client, s.sbNs.TokenProvider)
	if err != nil {
		return nil
	}

	lockIds := []amqp.UUID{}
	for _, mid := range ids {
		if id, ok := mid.(*uuid.UUID); ok {
			lockTokenBytes := [16]byte(*id)
			lockIds = append(lockIds, amqp.UUID(lockTokenBytes))
		}
	}

	value := map[string]interface{}{
		"disposition-status": completedStatus,
		"lock-tokens":        lockIds,
	}

	msg := &amqp.Message{
		ApplicationProperties: map[string]interface{}{
			"operation": "com.microsoft:update-disposition",
		},
		Value: value,
	}

	link, err := rpc.NewLink(client, s.sbSub.ManagementPath())
	if err != nil {
		return err
	}
	_, err = link.RetryableRPC(ctx, rpcTries, rpcRetryDelay, msg)
	return err
}

func errorCode(err error) gcerrors.ErrorCode {
	aerr, ok := err.(*amqp.Error)
	if !ok {
		return gcerrors.Unknown
	}
	switch aerr.Condition {
	case amqp.ErrorCondition(servicebus.ErrorNotFound):
		return gcerrors.NotFound

	case amqp.ErrorCondition(servicebus.ErrorPreconditionFailed):
		return gcerrors.FailedPrecondition

	case amqp.ErrorCondition(servicebus.ErrorInternalError):
		return gcerrors.Internal

	case amqp.ErrorCondition(servicebus.ErrorNotImplemented):
		return gcerrors.Unimplemented

	case amqp.ErrorCondition(servicebus.ErrorUnauthorizedAccess), amqp.ErrorCondition(servicebus.ErrorNotAllowed):
		return gcerrors.PermissionDenied

	case amqp.ErrorCondition(servicebus.ErrorResourceLimitExceeded):
		return gcerrors.ResourceExhausted

	case amqp.ErrorCondition(servicebus.ErrorInvalidField):
		return gcerrors.InvalidArgument

	default:
		return gcerrors.Unknown
	}
}

// AckFunc implements driver.Subscription.AckFunc.
func (*subscription) AckFunc() func() { return nil }
