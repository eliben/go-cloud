// Copyright 2019 The Go Cloud Development Kit Authors
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

package awspubsub_test

import (
	"context"
	"log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/aws/aws-sdk-go/service/sqs"
	"gocloud.dev/pubsub"
	"gocloud.dev/pubsub/awspubsub"
)

func ExampleOpenTopic() {
	ctx := context.Background()

	// Establish an AWS session.
	// See https://docs.aws.amazon.com/sdk-for-go/api/aws/session/ for more info.
	// The region must match the region where your topic is provisioned.
	session, err := session.NewSession(&aws.Config{Region: aws.String("us-west-1")})
	if err != nil {
		log.Fatal(err)
	}
	// Establish an SNS client.
	client := sns.New(session)

	// Construct a *pubsub.Topic.
	// https://docs.aws.amazon.com/gettingstarted/latest/deploy/creating-an-sns-topic.html
	topicARN := "arn:aws:service:region:accountid:resourceType/resourcePath"
	t := awspubsub.OpenTopic(ctx, client, topicARN, nil)
	defer t.Shutdown(ctx)

	// Now we can use t to send messages.
	err = t.Send(ctx, &pubsub.Message{Body: []byte("example message")})
}

func ExampleOpenSubscription() {
	ctx := context.Background()

	// Establish an AWS session.
	// See https://docs.aws.amazon.com/sdk-for-go/api/aws/session/ for more info.
	// The region must match the region where your topic is provisioned.
	session, err := session.NewSession(&aws.Config{Region: aws.String("us-west-1")})
	if err != nil {
		log.Fatal(err)
	}
	// Establish an SQS client.
	client := sqs.New(session)

	// Construct a *pubsub.Subscription.
	// https://docs.aws.amazon.com/sdk-for-net/v2/developer-guide/QueueURL.html
	queueURL := "https://region-endpoint/queue/account-number/queue-name"
	s := awspubsub.OpenSubscription(ctx, client, queueURL, nil)
	defer s.Shutdown(ctx)

	// Now we can use s to receive messages.
	msg, err := s.Receive(ctx)
	if err != nil {
		// Handle error....
	}
	// Handle message....
	msg.Ack()
}

func Example_openfromURL() {
	ctx := context.Background()

	// OpenTopic creates a *pubsub.Topic from a URL.
	// This URL will open the topic with the topic ARN "arn:aws:service:region:accountid:resourceType/resourcePath".
	t, err := pubsub.OpenTopic(ctx, "awssnssqs://arn:aws:service:region:accountid:resourceType/resourcePath")

	// Similarly, OpenSubscription creates a *pubsub.Subscription from a URL.
	// This URL will open the subscription with the URL "https://awssnssqs://sqs.us-east-2.amazonaws.com/99999/my-subscription".
	s, err := pubsub.OpenSubscription(ctx, "awssnssqs://sqs.us-east-2.amazonaws.com/99999/my-subscription")
	_, _, _ = t, s, err
}
