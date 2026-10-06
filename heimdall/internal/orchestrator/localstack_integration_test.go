//go:build integration

package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/queue"
	"github.com/heimdall-dev/heimdall/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestLocalStackFIFOAndPostgresDurableReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	local, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{Image: "public.ecr.aws/localstack/localstack:4.14.0", Env: map[string]string{"SERVICES": "sqs", "SQS_ENDPOINT_STRATEGY": "path", "EAGER_SERVICE_LOADING": "1"}, ExposedPorts: []string{"4566/tcp"}, WaitingFor: wait.ForHTTP("/_localstack/health").WithPort("4566/tcp").WithStartupTimeout(2 * time.Minute)}, Started: true})
	if err != nil {
		t.Fatalf("required LocalStack failed: %v", err)
	}
	defer func() { _ = local.Terminate(context.Background()) }()
	host, err := local.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := local.MappedPort(ctx, "4566/tcp")
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.New(sqs.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://" + host + ":" + port.Port()), Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
	})})
	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("heimdall-test-dlq.fifo"), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	dlqAttrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: dlq.QueueUrl, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	redrive, _ := json.Marshal(map[string]any{"deadLetterTargetArn": dlqAttrs.Attributes["QueueArn"], "maxReceiveCount": 2})
	created, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("heimdall-test.fifo"), Attributes: map[string]string{"FifoQueue": "true", "VisibilityTimeout": "120", "RedrivePolicy": string(redrive)}})
	if err != nil {
		t.Fatal(err)
	}
	q := &queue.SQS{Client: client, URL: aws.ToString(created.QueueUrl)}
	pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{Image: "public.ecr.aws/docker/library/postgres:17-alpine", Env: map[string]string{"POSTGRES_PASSWORD": "postgres", "POSTGRES_DB": "heimdall"}, ExposedPorts: []string{"5432/tcp"}, WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(time.Minute)}, Started: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Terminate(context.Background()) }()
	pghost, _ := pg.Host(ctx)
	pgport, _ := pg.MappedPort(ctx, "5432/tcp")
	operatorDSN := fmt.Sprintf("postgres://postgres:postgres@%s:%s/heimdall?sslmode=disable", pghost, pgport.Port())
	if err = store.Migrate(ctx, operatorDSN); err != nil {
		t.Fatal(err)
	}
	operator, err := pgx.Connect(ctx, operatorDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(context.Background())
	if _, err = operator.Exec(ctx, "ALTER ROLE heimdall_app PASSWORD 'test-app'"); err != nil {
		t.Fatal(err)
	}
	s, mem, g := setup(t)
	tenantID := uuid.NewString()
	policy, _ := json.Marshal(mem.policy)
	if err = store.ProvisionTenant(ctx, operatorDSN, domain.Tenant{ID: tenantID, Slug: "team", Name: "test"}, policy, domain.Quota{MaxEnvironments: 10, MaxCPUMilli: 6000, MaxMemoryMi: 12288, MaxStorageMi: 5120}); err != nil {
		t.Fatal(err)
	}
	appDSN := strings.Replace(operatorDSN, "postgres:postgres@", "heimdall_app:test-app@", 1)
	db, err := store.New(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := principal(tenantID, "operator")
	cluster, err := db.CreateCluster(ctx, p, domain.Cluster{Name: "customer", Tier: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.PutInstallation(ctx, p, domain.Installation{ID: 9, Account: "team"}); err != nil {
		t.Fatal(err)
	}
	repo := mem.repo
	repo.ID = ""
	repo.ClusterID = cluster.ID
	repo, err = db.PutRepository(ctx, p, repo)
	if err != nil {
		t.Fatal(err)
	}
	s.o.Store = db
	first := message("fifo-first", "pull_request")
	first.TraceParent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	second := message("fifo-second", "pull_request")
	other := message("fifo-other", "pull_request")
	other.PullRequest = 102
	for _, m := range []queue.Message{first, first, second, other} {
		if err = q.Send(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	seen := []string{}
	for len(seen) < 3 {
		received, err := q.Receive(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(received) == 0 {
			t.Fatal("FIFO messages disappeared")
		}
		for _, item := range received {
			seen = append(seen, item.DeliveryID)
			if item.PullRequest == 102 {
				g.pr.Number = 102
			} else {
				g.pr.Number = 101
			}
			if err = s.Process(ctx, item.Message); err != nil {
				t.Fatal(err)
			}
			if err = q.Ack(ctx, item); err != nil {
				t.Fatal(err)
			}
		}
	}
	index := func(id string) int {
		for i, v := range seen {
			if v == id {
				return i
			}
		}
		return -1
	}
	if index(first.DeliveryID) < 0 || index(first.DeliveryID) >= index(second.DeliveryID) {
		t.Fatalf("per-PR ordering failed: %v", seen)
	}
	var count int
	if err = operator.QueryRow(ctx, "SELECT count(*) FROM heimdall.webhook_deliveries WHERE tenant_id=$1", tenantID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("SQS duplicate not suppressed: %d %v", count, err)
	}
	envA, err := db.EnvironmentByPullRequest(ctx, p, repo.ID, 101)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPR, err := db.GetPullRequest(ctx, p, repo.ID, 101)
	if err != nil || canonicalPR.TraceParent == "" {
		t.Fatalf("durable trace identity was lost: %v %+v", err, canonicalPR)
	}
	envB, err := db.EnvironmentByPullRequest(ctx, p, repo.ID, 102)
	if err != nil || envA.ID == envB.ID {
		t.Fatal("PR environments are not isolated")
	}
	// Emulate redelivery after SQS's five-minute window by changing only its
	// transport deduplication ID. PostgreSQL must suppress the original ID.
	raw, _ := json.Marshal(first)
	_, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: aws.String(string(raw)), MessageGroupId: aws.String(first.Group()), MessageDeduplicationId: aws.String("outside-sqs-window")})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := q.Receive(ctx, 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("replay receive failed %v", err)
	}
	g.pr.Number = 101
	if err = s.Process(ctx, batch[0].Message); err != nil {
		t.Fatal(err)
	}
	if err = q.Ack(ctx, batch[0]); err != nil {
		t.Fatal(err)
	}
	after, err := db.EnvironmentByPullRequest(ctx, p, repo.ID, 101)
	if err != nil || after.Version != envA.Version || after.Generation != envA.Generation {
		t.Fatal("durable replay mutated state after transport dedupe expired")
	}
	if _, err = operator.Exec(ctx, "UPDATE heimdall.webhook_deliveries SET available_at=now() WHERE delivery_id='fifo-first'"); err != nil {
		t.Fatal(err)
	}
	// Closing one PR preserves the other, and a later stale synchronize is
	// interpreted through the current canonical GitHub PR state.
	g.pr.State = "closed"
	g.pr.UpdatedAt = g.pr.UpdatedAt.Add(time.Minute)
	if err = s.Process(ctx, message("close-a", "pull_request")); err != nil {
		t.Fatal(err)
	}
	if err = s.Process(ctx, message("late-open-a", "pull_request")); err != nil {
		t.Fatal(err)
	}
	closed, _ := db.EnvironmentByPullRequest(ctx, p, repo.ID, 101)
	survivor, _ := db.EnvironmentByPullRequest(ctx, p, repo.ID, 102)
	if closed.DesiredState != "Destroyed" || survivor.DesiredState != "Running" {
		t.Fatal("close/stale replay affected unrelated PR")
	}
	// A repeatedly failed FIFO item is routed to its real DLQ.
	poison := message("poison", "pull_request")
	if err = q.Send(ctx, poison); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		batch, err = q.Receive(ctx, 1)
		if err != nil || len(batch) != 1 {
			t.Fatalf("poison receive: %v", err)
		}
		if err = q.Extend(ctx, batch[0], 0); err != nil {
			t.Fatal(err)
		}
	}
	_, err = client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: created.QueueUrl, WaitTimeSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: dlq.QueueUrl, WaitTimeSeconds: 2})
	if err != nil || len(dead.Messages) != 1 {
		t.Fatalf("FIFO DLQ redrive failed %v", err)
	}
}
