package cloudwatchwriter

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/pkg/errors"
	"gopkg.in/oleiade/lane.v1"
)

const (
	// minBatchInterval is 200 ms as the maximum rate of PutLogEvents is 5
	// requests per second.
	minBatchInterval time.Duration = 200000000
	// defaultBatchInterval is 5 seconds.
	defaultBatchInterval time.Duration = 5000000000
	// batchSizeLimit is 1MB in bytes, the limit imposed by AWS CloudWatch Logs
	// on the size the batch of logs we send, see:
	// https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_PutLogEvents.html
	batchSizeLimit = 1048576
	// maxNumLogEvents is the maximum number of messages that can be sent in one
	// batch, also an AWS limitation, see:
	// https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_PutLogEvents.html
	maxNumLogEvents = 10000
	// additionalBytesPerLogEvent is the number of additional bytes per log
	// event, other than the length of the log message, see:
	// https://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_PutLogEvents.html
	additionalBytesPerLogEvent = 26
)

// CloudWatchLogsClient represents the AWS cloudwatchlogs client that we need to talk to CloudWatch
type CloudWatchLogsClient interface {
	DescribeLogStreams(ctx context.Context, params *cloudwatchlogs.DescribeLogStreamsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogStreamsOutput, error)
	CreateLogGroup(ctx context.Context, params *cloudwatchlogs.CreateLogGroupInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error)
	CreateLogStream(ctx context.Context, params *cloudwatchlogs.CreateLogStreamInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error)
	PutLogEvents(ctx context.Context, params *cloudwatchlogs.PutLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error)
}

// CloudWatchWriter can be inserted into zerolog to send logs to CloudWatch.
type CloudWatchWriter struct {
	sync.RWMutex
	client            CloudWatchLogsClient
	batchInterval     time.Duration
	queue             *lane.Queue
	err               error
	logGroupName      *string
	logStreamName     *string
	nextSequenceToken *string
	closing           bool
	done              chan struct{}
}

// New returns a pointer to a CloudWatchWriter struct, or an error.
func New(cfg aws.Config, logGroupName, logStreamName string) (*CloudWatchWriter, error) {
	return NewWithClient(cloudwatchlogs.NewFromConfig(cfg), defaultBatchInterval, logGroupName, logStreamName)
}

// NewWithClient returns a pointer to a CloudWatchWriter struct, or an error.
func NewWithClient(client CloudWatchLogsClient, batchInterval time.Duration, logGroupName, logStreamName string) (*CloudWatchWriter, error) {
	writer := &CloudWatchWriter{
		client:        client,
		queue:         lane.NewQueue(),
		logGroupName:  aws.String(logGroupName),
		logStreamName: aws.String(logStreamName),
		done:          make(chan struct{}),
	}

	err := writer.SetBatchInterval(batchInterval)
	if err != nil {
		return nil, errors.Wrapf(err, "set batch interval: %v", batchInterval)
	}

	logStream, err := writer.getOrCreateLogStream()
	if err != nil {
		return nil, err
	}
	writer.setNextSequenceToken(logStream.UploadSequenceToken)

	go writer.queueMonitor()

	return writer, nil
}

// SetBatchInterval sets the maximum time between batches of logs sent to
// CloudWatch.
func (c *CloudWatchWriter) SetBatchInterval(interval time.Duration) error {
	if interval < minBatchInterval {
		return errors.New("supplied batch interval is less than the minimum")
	}

	c.setBatchInterval(interval)
	return nil
}

func (c *CloudWatchWriter) setBatchInterval(interval time.Duration) {
	c.Lock()
	defer c.Unlock()

	c.batchInterval = interval
}

func (c *CloudWatchWriter) getBatchInterval() time.Duration {
	c.RLock()
	defer c.RUnlock()

	return c.batchInterval
}

func (c *CloudWatchWriter) setErr(err error) {
	c.Lock()
	defer c.Unlock()

	c.err = err
}

func (c *CloudWatchWriter) getErr() error {
	c.RLock()
	defer c.RUnlock()

	return c.err
}

func (c *CloudWatchWriter) setNextSequenceToken(next *string) {
	c.Lock()
	defer c.Unlock()

	c.nextSequenceToken = next
}

func (c *CloudWatchWriter) getNextSequenceToken() *string {
	c.RLock()
	defer c.RUnlock()

	return c.nextSequenceToken
}

// Write implements the io.Writer interface.
func (c *CloudWatchWriter) Write(log []byte) (int, error) {
	event := &types.InputLogEvent{
		Message: aws.String(string(log)),
		// Timestamp has to be in milliseconds since the epoch
		Timestamp: aws.Int64(time.Now().UTC().UnixNano() / int64(time.Millisecond)),
	}
	c.queue.Enqueue(event)

	// report last sending error
	lastErr := c.getErr()
	if lastErr != nil {
		c.setErr(nil)
		return 0, lastErr
	}
	return len(log), nil
}

func (c *CloudWatchWriter) queueMonitor() {
	var batch []types.InputLogEvent
	batchSize := 0
	nextSendTime := time.Now().Add(c.getBatchInterval())

	for {
		if time.Now().After(nextSendTime) {
			c.sendBatch(batch, 0)
			batch = nil
			batchSize = 0
			nextSendTime.Add(c.getBatchInterval())
		}

		item := c.queue.Dequeue()
		if item == nil {
			// Empty queue, means no logs to process
			if c.isClosing() {
				c.sendBatch(batch, 0)
				// At this point we've processed all the logs and can safely
				// close.
				close(c.done)
				return
			}
			time.Sleep(time.Millisecond)
			continue
		}

		logEvent, ok := item.(*types.InputLogEvent)
		if !ok || logEvent.Message == nil {
			// This should not happen!
			continue
		}

		messageSize := len(*logEvent.Message) + additionalBytesPerLogEvent
		// Send the batch before adding the next message, if the message would
		// push it over the 1MB limit on batch size.
		if batchSize+messageSize > batchSizeLimit {
			c.sendBatch(batch, 0)
			batch = nil
			batchSize = 0
			nextSendTime = time.Now().Add(c.getBatchInterval())
		}

		batch = append(batch, *logEvent)
		batchSize += messageSize

		if len(batch) >= maxNumLogEvents {
			c.sendBatch(batch, 0)
			batch = nil
			batchSize = 0
			nextSendTime = time.Now().Add(c.getBatchInterval())
		}
	}
}

// Only allow 1 retry of an invalid sequence token.
func (c *CloudWatchWriter) sendBatch(batch []types.InputLogEvent, retryNum int) {
	if retryNum > 1 || len(batch) == 0 {
		return
	}

	input := &cloudwatchlogs.PutLogEventsInput{
		LogEvents:     batch,
		LogGroupName:  c.logGroupName,
		LogStreamName: c.logStreamName,
		SequenceToken: c.getNextSequenceToken(),
	}

	output, err := c.client.PutLogEvents(context.TODO(), input)
	if err != nil {
		var ist *types.InvalidSequenceTokenException
		if errors.As(err, &ist) {
			c.setNextSequenceToken(ist.ExpectedSequenceToken)
			c.sendBatch(batch, retryNum+1)
			return
		}
		c.setErr(err)
		return
	}
	c.setNextSequenceToken(output.NextSequenceToken)
}

// Close blocks until the writer has completed writing the logs to CloudWatch.
func (c *CloudWatchWriter) Close() {
	c.setClosing()
	// block until the done channel is closed
	<-c.done
}

func (c *CloudWatchWriter) isClosing() bool {
	c.RLock()
	defer c.RUnlock()

	return c.closing
}

func (c *CloudWatchWriter) setClosing() {
	c.Lock()
	defer c.Unlock()

	c.closing = true
}

// getOrCreateLogStream gets info on the log stream for the log group and log
// stream we're interested in -- primarily for the purpose of finding the value
// of the next sequence token. If the log group doesn't exist, then we create
// it, if the log stream doesn't exist, then we create it.
func (c *CloudWatchWriter) getOrCreateLogStream() (*types.LogStream, error) {
	// Get the log streams that match our log group name and log stream
	output, err := c.client.DescribeLogStreams(context.TODO(), &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        c.logGroupName,
		LogStreamNamePrefix: c.logStreamName,
	})
	if err != nil || output == nil {
		var rnf *types.ResourceNotFoundException
		if errors.As(err, &rnf) {
			_, err = c.client.CreateLogGroup(context.TODO(), &cloudwatchlogs.CreateLogGroupInput{
				LogGroupName: c.logGroupName,
			})
			if err != nil {
				return nil, errors.Wrap(err, "cloudwatchlog.Client.CreateLogGroup")
			}
			return c.getOrCreateLogStream()
		}
		return nil, errors.Wrap(err, "cloudwatchlogs.Client.DescribeLogStreams")
	}

	if len(output.LogStreams) > 0 {
		return &output.LogStreams[0], nil
	}

	// No matching log stream, so we need to create it
	_, err = c.client.CreateLogStream(context.TODO(), &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  c.logGroupName,
		LogStreamName: c.logStreamName,
	})
	if err != nil {
		return nil, errors.Wrap(err, "cloudwatchlogs.Client.CreateLogStream")
	}

	// We can just return an empty log stream as the initial sequence token would be nil anyway.
	return &types.LogStream{}, nil
}
