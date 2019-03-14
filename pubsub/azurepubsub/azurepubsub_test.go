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
package azurepubsub

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"gocloud.dev/internal/testing/setup"
	"gocloud.dev/pubsub"
	"gocloud.dev/pubsub/driver"
	"gocloud.dev/pubsub/drivertest"

	"github.com/Azure/azure-amqp-common-go"
	"github.com/Azure/azure-service-bus-go"
)

var (
	// See docs below on how to provision an Azure Service Bus Namespace and obtaining the connection string.
	// https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-dotnet-get-started-with-queues
	connString = os.Getenv("SERVICEBUS_CONNECTION_STRING")
)

const (
	topicName = "test-topic"
)

type harness struct {
	ns        *servicebus.Namespace
	numTopics uint32 // atomic
	numSubs   uint32 // atomic
	closer    func()
}

func newHarness(ctx context.Context, t *testing.T) (drivertest.Harness, error) {
	if connString == "" {
		return nil, fmt.Errorf("azurepubsub: test harness requires environment variable SERVICEBUS_CONNECTION_STRING to run")
	}

	ns, err := NewNamespaceFromConnectionString(connString)
	if err != nil {
		return nil, err
	}

	noop := func() {

	}

	return &harness{
		ns:     ns,
		closer: noop,
	}, nil
}

func (h *harness) CreateTopic(ctx context.Context, testName string) (dt driver.Topic, cleanup func(), err error) {
	topicName := fmt.Sprintf("%s-topic-%d", sanitize(testName), atomic.AddUint32(&h.numTopics, 1))

	createTopic(ctx, topicName, h.ns, nil)

	sbTopic, err := NewTopic(h.ns, topicName, nil)
	dt = openTopic(ctx, sbTopic)

	cleanup = func() {
		sbTopic.Close(ctx)
		deleteTopic(ctx, topicName, h.ns)
	}

	return dt, cleanup, nil
}

func (h *harness) MakeNonexistentTopic(ctx context.Context) (driver.Topic, error) {
	sbTopic, err := NewTopic(h.ns, topicName, nil)
	if err != nil {
		return nil, err
	}
	dt := openTopic(ctx, sbTopic)
	return dt, nil
}

func (h *harness) CreateSubscription(ctx context.Context, dt driver.Topic, testName string) (ds driver.Subscription, cleanup func(), err error) {
	// Keep the subscription entity name under 50 characters as per Azure limits.
	// See https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-quotas
	subName := fmt.Sprintf("%s-sub-%d", sanitize(testName), atomic.AddUint32(&h.numSubs, 1))
	if len(subName) > 50 {
		subName = subName[:50]
	}

	t := dt.(*topic)

	err = createSubscription(ctx, t.sbTopic.Name, subName, h.ns, nil)
	if err != nil {
		return nil, nil, err
	}

	sbSub, err := NewSubscription(t.sbTopic, subName, nil)
	if err != nil {
		return nil, nil, err
	}

	ds = openSubscription(ctx, h.ns, t.sbTopic, sbSub, nil)

	cleanup = func() {
		sbSub.Close(ctx)
		deleteSubscription(ctx, t.sbTopic.Name, subName, h.ns)
	}

	return ds, cleanup, nil
}

func (h *harness) MakeNonexistentSubscription(ctx context.Context) (driver.Subscription, error) {
	sbTopic, _ := NewTopic(h.ns, topicName, nil)
	sbSub, _ := NewSubscription(sbTopic, "nonexistent-subscription", nil)
	ds := openSubscription(ctx, h.ns, sbTopic, sbSub, nil)
	return ds, nil
}

func (h *harness) Close() {
	h.closer()
}

// Please run the TestConformance with an extended timeout since each test needs to preform CRUD for ServiceBus Topics and Subscriptions.
// Example: C:\Go\bin\go.exe test -timeout 60s gocloud.dev/pubsub/azurepubsub -run ^TestConformance$
func TestConformance(t *testing.T) {
	if !*setup.Record {
		t.Skip("replaying is not yet supported for Azure pubsub")

	} else {
		asTests := []drivertest.AsTest{sbAsTest{}}
		drivertest.RunConformanceTests(t, newHarness, asTests)
	}
}

type sbAsTest struct{}

func (sbAsTest) Name() string {
	return "azure"
}

func (sbAsTest) TopicCheck(top *pubsub.Topic) error {
	var t2 servicebus.Topic
	if top.As(&t2) {
		return fmt.Errorf("cast succeeded for %T, want failure", &t2)
	}
	var t3 *servicebus.Topic
	if !top.As(&t3) {
		return fmt.Errorf("cast failed for %T", &t3)
	}
	return nil
}

func (sbAsTest) SubscriptionCheck(sub *pubsub.Subscription) error {
	var s2 servicebus.Subscription
	if sub.As(&s2) {
		return fmt.Errorf("cast succeeded for %T, want failure", &s2)
	}
	var s3 *servicebus.Subscription
	if !sub.As(&s3) {
		return fmt.Errorf("cast failed for %T", &s3)
	}
	return nil
}

func (sbAsTest) TopicErrorCheck(t *pubsub.Topic, err error) error {
	var sbError common.Retryable
	if !t.ErrorAs(err, &sbError) {
		return fmt.Errorf("failed to convert %v (%T) to a common.Retryable", err, err)
	}
	return nil
}

func (sbAsTest) SubscriptionErrorCheck(s *pubsub.Subscription, err error) error {
	// We generate our own error for non-existent subscription, so there's no
	// underlying Azure error type.
	return nil
}

func (sbAsTest) MessageCheck(m *pubsub.Message) error {
	var m2 servicebus.Message
	if m.As(&m2) {
		return fmt.Errorf("cast succeeded for %T, want failure", &m2)
	}
	var m3 *servicebus.Message
	if !m.As(&m3) {
		return fmt.Errorf("cast failed for %T", &m3)
	}
	return nil
}

func sanitize(testName string) string {
	return strings.Replace(testName, "/", "_", -1)
}

// createTopic ensures the existance of a Service Bus Topic on a given Namespace.
func createTopic(ctx context.Context, topicName string, ns *servicebus.Namespace, opts []servicebus.TopicManagementOption) error {
	tm := ns.NewTopicManager()
	_, err := tm.Get(ctx, topicName)
	if err == nil {
		_ = tm.Delete(ctx, topicName)
	}
	_, err = tm.Put(ctx, topicName, opts...)
	return err
}

// deleteTopic removes a Service Bus Topic on a given Namespace.
func deleteTopic(ctx context.Context, topicName string, ns *servicebus.Namespace) error {
	tm := ns.NewTopicManager()
	te, _ := tm.Get(ctx, topicName)
	if te != nil {
		return tm.Delete(ctx, topicName)
	}
	return nil
}

// createTopic ensures the existance of a Service Bus Subscription on a given Namespace and Topic.
func createSubscription(ctx context.Context, topicName string, subscriptionName string, ns *servicebus.Namespace, opts []servicebus.SubscriptionManagementOption) error {
	sm, err := ns.NewSubscriptionManager(topicName)
	if err != nil {
		return err
	}
	_, err = sm.Get(ctx, subscriptionName)
	if err == nil {
		_ = sm.Delete(ctx, subscriptionName)
	}
	_, err = sm.Put(ctx, subscriptionName, opts...)
	return err
}

// deleteSubscription removes a Service Bus Subscription on a given Namespace and Topic.
func deleteSubscription(ctx context.Context, topicName string, subscriptionName string, ns *servicebus.Namespace) error {
	sm, err := ns.NewSubscriptionManager(topicName)
	if err != nil {
		return nil
	}
	se, _ := sm.Get(ctx, subscriptionName)
	if se != nil {
		_ = sm.Delete(ctx, subscriptionName)
	}
	return nil
}

func fakeConnectionStringInEnv() func() {
	oldEnvVal := os.Getenv("SERVICEBUS_CONNECTION_STRING")
	os.Setenv("SERVICEBUS_CONNECTION_STRING", "Endpoint=sb://foo.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=mykey")
	return func() {
		os.Setenv("SERVICEBUS_CONNECTION_STRING", oldEnvVal)
	}
}

func TestOpenTopicFromURL(t *testing.T) {
	cleanup := fakeConnectionStringInEnv()
	defer cleanup()

	tests := []struct {
		URL     string
		WantErr bool
	}{
		// OK.
		{"azuresb://mytopic", false},
		// Invalid parameter.
		{"azuresb://mytopic?param=value", true},
	}

	ctx := context.Background()
	for _, test := range tests {
		_, err := pubsub.OpenTopic(ctx, test.URL)
		if (err != nil) != test.WantErr {
			t.Errorf("%s: got error %v, want error %v", test.URL, err, test.WantErr)
		}
	}
}

func TestOpenSubscriptionFromURL(t *testing.T) {
	cleanup := fakeConnectionStringInEnv()
	defer cleanup()

	tests := []struct {
		URL     string
		WantErr bool
	}{
		// OK.
		{"azuresb://mytopic?subscription=mysub", false},
		// Missing subscription.
		{"azuresb://mytopic", true},
		// Invalid parameter.
		{"azuresb://mytopic?subscription=mysub&param=value", true},
	}

	ctx := context.Background()
	for _, test := range tests {
		_, err := pubsub.OpenSubscription(ctx, test.URL)
		if (err != nil) != test.WantErr {
			t.Errorf("%s: got error %v, want error %v", test.URL, err, test.WantErr)
		}
	}
}
