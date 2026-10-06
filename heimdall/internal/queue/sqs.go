// Package queue transports verified GitHub events. FIFO supplies ordering;
// PostgreSQL's inbox supplies durable deduplication beyond SQS's time window.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
)

const MaxBodyBytes = 240 << 10

type Message struct {
	DeliveryID     string          `json:"deliveryId"`
	Event          string          `json:"event"`
	InstallationID int64           `json:"installationId"`
	RepositoryID   int64           `json:"repositoryId"`
	PullRequest    int64           `json:"pullRequest"`
	Payload        json.RawMessage `json:"payload"`
	TraceParent    string          `json:"traceParent,omitempty"`
}

func (m Message) Group() string {
	return strconv.FormatInt(m.RepositoryID, 10) + "#" + strconv.FormatInt(m.PullRequest, 10)
}
func (m Message) Validate() error {
	if m.DeliveryID == "" || len(m.DeliveryID) > 128 || m.InstallationID <= 0 || m.RepositoryID <= 0 || m.PullRequest <= 0 {
		return errors.New("incomplete webhook queue identity")
	}
	if m.Event != "pull_request" && m.Event != "issue_comment" {
		return errors.New("unsupported webhook event")
	}
	if len(m.Payload) > MaxBodyBytes || !json.Valid(m.Payload) {
		return errors.New("invalid or oversized webhook payload")
	}
	if err := tracecontext.Validate(m.TraceParent); err != nil {
		return err
	}
	return nil
}

type API interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}
type SQS struct {
	Client API
	URL    string
}
type Received struct {
	Message
	Receipt  string
	Attempts int
}

func (q *SQS) Send(ctx context.Context, m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > 256<<10 {
		return errors.New("webhook queue envelope exceeds 256 KiB")
	}
	digest := sha256.Sum256([]byte(m.DeliveryID))
	_, err = q.Client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(q.URL), MessageBody: aws.String(string(b)),
		MessageGroupId: aws.String(m.Group()), MessageDeduplicationId: aws.String(hex.EncodeToString(digest[:]))})
	return err
}
func (q *SQS) Receive(ctx context.Context, limit int32) ([]Received, error) {
	if limit < 1 || limit > 10 {
		limit = 10
	}
	r, err := q.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(q.URL), MaxNumberOfMessages: limit,
		WaitTimeSeconds: 20, VisibilityTimeout: 120, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount, types.MessageSystemAttributeNameMessageGroupId}})
	if err != nil {
		return nil, err
	}
	out := make([]Received, 0, len(r.Messages))
	for _, m := range r.Messages {
		var env Message
		if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &env); err != nil {
			return nil, fmt.Errorf("invalid SQS envelope: %w", err)
		}
		if err := env.Validate(); err != nil {
			return nil, err
		}
		if group := m.Attributes["MessageGroupId"]; group != "" && group != env.Group() {
			return nil, errors.New("SQS FIFO group does not match webhook identity")
		}
		attempts, _ := strconv.Atoi(m.Attributes["ApproximateReceiveCount"])
		out = append(out, Received{Message: env, Receipt: aws.ToString(m.ReceiptHandle), Attempts: attempts})
	}
	return out, nil
}
func (q *SQS) Ack(ctx context.Context, r Received) error {
	_, err := q.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(q.URL), ReceiptHandle: aws.String(r.Receipt)})
	return err
}
func (q *SQS) Extend(ctx context.Context, r Received, seconds int32) error {
	_, err := q.Client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(q.URL), ReceiptHandle: aws.String(r.Receipt), VisibilityTimeout: seconds})
	return err
}
