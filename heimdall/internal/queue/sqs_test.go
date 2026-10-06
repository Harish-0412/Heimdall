package queue

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type captureSQS struct {
	API
	sent     []*sqs.SendMessageInput
	messages []types.Message
}

func (c *captureSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	c.sent = append(c.sent, in)
	return &sqs.SendMessageOutput{}, nil
}
func (c *captureSQS) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if in.VisibilityTimeout != 120 {
		panic("worker lease exceeds visibility timeout")
	}
	return &sqs.ReceiveMessageOutput{Messages: c.messages}, nil
}
func TestFIFOIdentityAndTraceBoundary(t *testing.T) {
	message := Message{DeliveryID: "delivery", Event: "pull_request", InstallationID: 9, RepositoryID: 7, PullRequest: 101, Payload: json.RawMessage(`{"action":"opened"}`), TraceParent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
	client := &captureSQS{}
	q := &SQS{Client: client, URL: "test"}
	if err := q.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(client.sent[0].MessageGroupId) != "7#101" {
		t.Fatal("wrong FIFO group")
	}
	if err := q.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(client.sent[0].MessageDeduplicationId) != aws.ToString(client.sent[1].MessageDeduplicationId) {
		t.Fatal("delivery identity does not survive queue retries")
	}
	body, _ := json.Marshal(message)
	client.messages = []types.Message{{Body: aws.String(string(body)), Attributes: map[string]string{"MessageGroupId": "7#102"}}}
	if _, err := q.Receive(context.Background(), 1); err == nil {
		t.Fatal("FIFO envelope with mismatched PR group accepted")
	}
	message.TraceParent = "invalid"
	if err := q.Send(context.Background(), message); err == nil {
		t.Fatal("invalid propagation context accepted")
	}
}
