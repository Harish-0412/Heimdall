#!/bin/sh
set -eu
awslocal sqs create-queue --queue-name heimdall-dlq.fifo --attributes FifoQueue=true >/dev/null
arn=$(awslocal sqs get-queue-attributes --queue-url http://localhost:4566/000000000000/heimdall-dlq.fifo --attribute-names QueueArn --query Attributes.QueueArn --output text)
awslocal sqs create-queue --queue-name heimdall.fifo --attributes "{\"FifoQueue\":\"true\",\"VisibilityTimeout\":\"120\",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$arn\\\",\\\"maxReceiveCount\\\":6}\"}" >/dev/null
